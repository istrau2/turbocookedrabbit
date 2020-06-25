package publisher

import (
	"fmt"
	"sync"
	"time"

	"github.com/houseofcat/turbocookedrabbit/models"
	"github.com/houseofcat/turbocookedrabbit/pools"

	"github.com/streadway/amqp"
)

// Publisher contains everything you need to publish a message.
type Publisher struct {
	Config                   *models.RabbitSeasoning
	ConnectionPool           *pools.ConnectionPool
	errors                   chan error
	letters                  chan *models.Letter
	autoStop                 chan bool
	publishReceipts          chan *models.PublishReceipt
	autoStarted              bool
	autoPublishGroup         *sync.WaitGroup
	sleepOnIdleInterval      time.Duration
	sleepOnQueueFullInterval time.Duration
	sleepOnErrorInterval     time.Duration
	pubLock                  *sync.Mutex
	pubRWLock                *sync.RWMutex
}

// NewPublisherWithConfig creates and configures a new Publisher.
func NewPublisherWithConfig(
	config *models.RabbitSeasoning,
	cp *pools.ConnectionPool) (*Publisher, error) {

	return &Publisher{
		Config:               config,
		ConnectionPool:       cp,
		errors:               make(chan error),
		letters:              make(chan *models.Letter),
		autoStop:             make(chan bool, 1),
		autoPublishGroup:     &sync.WaitGroup{},
		publishReceipts:      make(chan *models.PublishReceipt),
		sleepOnIdleInterval:  time.Duration(config.PublisherConfig.SleepOnIdleInterval) * time.Millisecond,
		sleepOnErrorInterval: time.Duration(config.PublisherConfig.SleepOnErrorInterval) * time.Millisecond,
		pubLock:              &sync.Mutex{},
		pubRWLock:            &sync.RWMutex{},
		autoStarted:          false,
	}, nil
}

// NewPublisher creates and configures a new Publisher.
func NewPublisher(
	cp *pools.ConnectionPool,
	sleepOnIdleInterval time.Duration,
	sleepOnErrorInterval time.Duration) (*Publisher, error) {

	return &Publisher{
		ConnectionPool:       cp,
		letters:              make(chan *models.Letter),
		autoStop:             make(chan bool, 1),
		autoPublishGroup:     &sync.WaitGroup{},
		publishReceipts:      make(chan *models.PublishReceipt),
		sleepOnIdleInterval:  sleepOnIdleInterval,
		sleepOnErrorInterval: sleepOnErrorInterval,
		pubLock:              &sync.Mutex{},
		pubRWLock:            &sync.RWMutex{},
		autoStarted:          false,
	}, nil
}

// Publish sends a single message to the address on the letter.
// Subscribe to PublishReceipts to see success and errors.
// For proper resilience (at least once delivery guarantee over shaky network) use PublishWithConfirmation
func (pub *Publisher) Publish(letter *models.Letter) {

	chanHost := pub.ConnectionPool.GetChannel(!pub.Config.PublisherConfig.AutoAck)

	pub.simplePublish(chanHost, letter)
}

func (pub *Publisher) simplePublish(chanHost *pools.ChannelHost, letter *models.Letter) {

	err := chanHost.Channel.Publish(
		letter.Envelope.Exchange,
		letter.Envelope.RoutingKey,
		letter.Envelope.Mandatory,
		letter.Envelope.Immediate,
		amqp.Publishing{
			ContentType:  letter.Envelope.ContentType,
			Body:         letter.Body,
			Headers:      amqp.Table(letter.Envelope.Headers),
			DeliveryMode: letter.Envelope.DeliveryMode,
		},
	)

	chanHost.Close()
	pub.publishReceipt(letter, err)
}

// PublishWithConfirmation sends a single message to the address on the letter with confirmation capabilities.
// This is an expensive and slow call - use this when delivery confirmation on publish is your highest priority.
// A timeout failure drops the letter back in the PublishReceipts.
// A confirmation failure keeps trying to publish (at least until timeout failure occurs.)
func (pub *Publisher) PublishWithConfirmation(letter *models.Letter, timeout time.Duration) {

	timeoutAfter := time.After(timeout)

GetChannelAndPublish:
	for {
		// Has to use an Ackable channel for Publish Confirmations.
		chanHost := pub.ConnectionPool.GetChannel(true)

		// Subscribe to publish confirmations
		chanHost.Channel.NotifyPublish(chanHost.Confirmations)

	Publish:
		err := chanHost.Channel.Publish(
			letter.Envelope.Exchange,
			letter.Envelope.RoutingKey,
			letter.Envelope.Mandatory,
			letter.Envelope.Immediate,
			amqp.Publishing{
				ContentType:  letter.Envelope.ContentType,
				Body:         letter.Body,
				Headers:      amqp.Table(letter.Envelope.Headers),
				DeliveryMode: letter.Envelope.DeliveryMode,
			},
		)
		if err != nil {
			chanHost.Close()
			continue // Take it again! From the top!
		}

		// Wait for Publish Confirmations
		for {
			select {
			case <-timeoutAfter:
				pub.publishReceipt(letter, fmt.Errorf("publish confirmation for LetterId: %d wasn't received in a timely manner (300ms) - recommend manual retry", letter.LetterID))
				break

			case confirmation := <-chanHost.Confirmations:

				if !confirmation.Ack { // retry publish
					goto Publish
				}

				pub.publishReceipt(letter, nil)
				break GetChannelAndPublish

			default:

				time.Sleep(time.Duration(time.Millisecond * 3))
			}
		}
	}
}

// PublishReceipts yields all the success and failures during all publish events. Highly recommend susbscribing to this.
func (pub *Publisher) PublishReceipts() <-chan *models.PublishReceipt {
	return pub.publishReceipts
}

// StartAutoPublishing starts the Publisher's auto-publishing capabilities.
func (pub *Publisher) StartAutoPublishing() {
	pub.pubLock.Lock()
	defer pub.pubLock.Unlock()

	if !pub.autoStarted {
		pub.FlushStops()

		pub.autoStarted = true
		go pub.startAutoPublishingLoop()
	}
}

// StartAutoPublish starts auto-publishing letters queued up - is locking.
func (pub *Publisher) startAutoPublishingLoop() {

AutoPublishLoop:
	for {
		// Detect if we should stop publishing.
		select {
		case stop := <-pub.autoStop:
			if stop {
				break AutoPublishLoop
			}
		default:
			break
		}

		// Get ChannelHost
		chanHost := pub.ConnectionPool.GetChannel(true)

		// Deliver letters queued in the publisher, returns true when we are to stop publishing.
		if pub.deliverLetters(chanHost) {
			chanHost.Close()
			break AutoPublishLoop
		}

		chanHost.Close()
	}

	pub.pubLock.Lock()
	pub.autoStarted = false
	pub.pubLock.Unlock()
}

func (pub *Publisher) deliverLetters(chanHost *pools.ChannelHost) bool {

DeliverLettersLoop:
	for {
		// Listen for channel closure (close errors).
		// Highest priority so separated to it's own select.
		select {
		case errorMessage := <-chanHost.Errors():
			if errorMessage != nil {
				pub.errors <- fmt.Errorf("autopublisher's current channel closed\r\n[reason: %s]\r\n[code: %d]", errorMessage.Reason, errorMessage.Code)
				break DeliverLettersLoop
			}
		default:
			break
		}

		// Publish the letter.
		select {
		case letter := <-pub.letters:
			pub.simplePublish(chanHost, letter)
		default:
			if pub.sleepOnIdleInterval > 0 {
				time.Sleep(pub.sleepOnIdleInterval)
			}
			break
		}

		// Detect if we should stop publishing.
		select {
		case stop := <-pub.autoStop:
			if stop {
				break DeliverLettersLoop
			}
		default:
			break
		}
	}

	return false
}

// StopAutoPublish stops publishing letters queued up.
func (pub *Publisher) StopAutoPublish() {
	pub.pubLock.Lock()
	defer pub.pubLock.Unlock()

	if !pub.autoStarted {
		return
	}

	go func() { pub.autoStop <- true }() // signal auto publish to stop
}

// QueueLetters allows you to bulk queue letters that will be consumed by AutoPublish.
// Blocks on the Letter Buffer being full.
func (pub *Publisher) QueueLetters(letters []*models.Letter) {

	for _, letter := range letters {

		pub.letters <- letter
	}
}

// QueueLetter queues up a letter that will be consumed by AutoPublish.
// Blocks on the Letter Buffer being full.
func (pub *Publisher) QueueLetter(letter *models.Letter) {

	pub.letters <- letter
}

// publishReceipt sends the status to the receipt channel.
func (pub *Publisher) publishReceipt(letter *models.Letter, err error) {

	publishReceipt := &models.PublishReceipt{
		LetterID: letter.LetterID,
		Error:    err,
	}

	if err == nil {
		publishReceipt.Success = true
	} else {
		publishReceipt.FailedLetter = letter
	}

	pub.publishReceipts <- publishReceipt
}

// Errors yields all the internal errs for delivering letters.
func (pub *Publisher) Errors() <-chan error {
	return pub.errors
}

// FlushStops flushes out all the AutoStop messages.
func (pub *Publisher) FlushStops() {

FlushLoop:
	for {
		select {
		case <-pub.autoStop:
		default:
			break FlushLoop
		}
	}
}

// Shutdown cleanly shutsdown the publisher and resets it's internal state.
func (pub *Publisher) Shutdown(shutdownPools bool) {
	pub.StopAutoPublish()

	if shutdownPools { // in case the ChannelPool is shared between structs, you can prevent it from shutting down
		pub.ConnectionPool.Shutdown()
	}
}
