package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"repolens/internal/platform/logger"
)

const (
	QueueDiagnosisTask = "repolens.diagnosis.task"
	QueueDiagnosisDLQ  = "repolens.diagnosis.dlq"
	QueueIndexTask     = "repolens.index.task"
	QueueIndexDLQ      = "repolens.index.dlq"

	ExchangeDirect = "repolens.direct"
	ExchangeDLX    = "repolens.dlx"
)

type Message struct {
	ID           string                   `json:"id"`
	EventType    string                   `json:"event_type"`
	Payload      string                   `json:"payload"`
	Headers      map[string]string        `json:"headers,omitempty"`
	Redelivered  bool                     `json:"redelivered"`
	AttemptCount int                      `json:"attempt_count"`
	AckFunc      func() error             `json:"-"`
	NackFunc     func(requeue bool) error `json:"-"`
}

type Broker interface {
	Publish(ctx context.Context, queue string, msg Message) error
	PublishDLQ(ctx context.Context, queue string, msg Message, reason string) error
	Consume(ctx context.Context, queue string, prefetch int) (<-chan Message, error)
	Close() error
}

type MemoryBroker struct {
	mu     sync.Mutex
	queues map[string]chan Message
	closed bool
}

func NewMemoryBroker() *MemoryBroker {
	return &MemoryBroker{
		queues: make(map[string]chan Message),
	}
}

func (b *MemoryBroker) getOrCreateQueue(queue string) chan Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.queues[queue]; ok {
		return ch
	}
	ch := make(chan Message, 500)
	b.queues[queue] = ch
	return ch
}

func (b *MemoryBroker) Publish(ctx context.Context, queue string, msg Message) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errors.New("broker closed")
	}
	b.mu.Unlock()

	ch := b.getOrCreateQueue(queue)
	select {
	case ch <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errors.New("queue buffer full")
	}
}

func (b *MemoryBroker) PublishDLQ(ctx context.Context, queue string, msg Message, reason string) error {
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}
	msg.Headers["x-death-reason"] = reason
	return b.Publish(ctx, queue+".dlq", msg)
}

func (b *MemoryBroker) Consume(ctx context.Context, queue string, prefetch int) (<-chan Message, error) {
	out := make(chan Message, prefetch)
	src := b.getOrCreateQueue(queue)

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-src:
				if !ok {
					return
				}
				msg.AckFunc = func() error { return nil }
				msg.NackFunc = func(requeue bool) error {
					if requeue {
						msg.Redelivered = true
						return b.Publish(context.Background(), queue, msg)
					}
					return nil
				}
				select {
				case out <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

func (b *MemoryBroker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, ch := range b.queues {
		close(ch)
	}
	return nil
}

type RabbitMQBroker struct {
	url      string
	conn     *amqp.Connection
	channel  *amqp.Channel
	mu       sync.Mutex
	isClosed bool
}

func NewRabbitMQBroker(url string) (*RabbitMQBroker, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("failed to dial rabbitmq: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to open rabbitmq channel: %w", err)
	}

	b := &RabbitMQBroker{
		url:     url,
		conn:    conn,
		channel: ch,
	}

	if err := b.initTopology(); err != nil {
		b.Close()
		return nil, fmt.Errorf("failed to initialize rabbitmq topology: %w", err)
	}

	return b, nil
}

func (b *RabbitMQBroker) initTopology() error {
	// Declare DLX
	if err := b.channel.ExchangeDeclare(ExchangeDLX, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	// Declare main direct exchange
	if err := b.channel.ExchangeDeclare(ExchangeDirect, "direct", true, false, false, false, nil); err != nil {
		return err
	}

	// Declare DLQs
	for _, dlq := range []string{QueueDiagnosisDLQ, QueueIndexDLQ} {
		if _, err := b.channel.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
			return err
		}
		if err := b.channel.QueueBind(dlq, dlq, ExchangeDLX, false, nil); err != nil {
			return err
		}
	}

	// Declare main queues with dead-letter configuration
	queues := []struct {
		name    string
		dlqName string
	}{
		{QueueDiagnosisTask, QueueDiagnosisDLQ},
		{QueueIndexTask, QueueIndexDLQ},
	}

	for _, q := range queues {
		args := amqp.Table{
			"x-dead-letter-exchange":    ExchangeDLX,
			"x-dead-letter-routing-key": q.dlqName,
		}
		if _, err := b.channel.QueueDeclare(q.name, true, false, false, false, args); err != nil {
			return err
		}
		if err := b.channel.QueueBind(q.name, q.name, ExchangeDirect, false, nil); err != nil {
			return err
		}
	}

	return nil
}

func (b *RabbitMQBroker) Publish(ctx context.Context, queue string, msg Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	headers := amqp.Table{}
	for k, v := range msg.Headers {
		headers[k] = v
	}

	return b.channel.PublishWithContext(ctx,
		ExchangeDirect,
		queue,
		false,
		false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			MessageId:    msg.ID,
			Timestamp:    time.Now(),
			Type:         msg.EventType,
			Headers:      headers,
			Body:         body,
		},
	)
}

func (b *RabbitMQBroker) PublishDLQ(ctx context.Context, queue string, msg Message, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	dlqQueue := queue
	if queue == QueueDiagnosisTask {
		dlqQueue = QueueDiagnosisDLQ
	} else if queue == QueueIndexTask {
		dlqQueue = QueueIndexDLQ
	}

	return b.channel.PublishWithContext(ctx,
		ExchangeDLX,
		dlqQueue,
		false,
		false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			MessageId:    msg.ID,
			Headers: amqp.Table{
				"x-death-reason": reason,
			},
			Body: body,
		},
	)
}

func (b *RabbitMQBroker) Consume(ctx context.Context, queue string, prefetch int) (<-chan Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.channel.Qos(prefetch, 0, false); err != nil {
		return nil, fmt.Errorf("failed to set qos: %w", err)
	}

	deliveries, err := b.channel.Consume(
		queue,
		"",
		false, // auto-ack false (explicit manual ACK required!)
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start consuming: %w", err)
	}

	out := make(chan Message, prefetch)

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-deliveries:
				if !ok {
					return
				}
				var msg Message
				if err := json.Unmarshal(d.Body, &msg); err != nil {
					logger.L(ctx).Error("failed to unmarshal delivery body, nacking to DLQ", "error", err)
					_ = d.Nack(false, false) // reject to DLQ
					continue
				}
				msg.Redelivered = d.Redelivered
				tag := d.DeliveryTag
				ch := b.channel
				msg.AckFunc = func() error {
					return ch.Ack(tag, false)
				}
				msg.NackFunc = func(requeue bool) error {
					return ch.Nack(tag, false, requeue)
				}

				select {
				case out <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

func (b *RabbitMQBroker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.isClosed = true
	if b.channel != nil {
		_ = b.channel.Close()
	}
	if b.conn != nil {
		return b.conn.Close()
	}
	return nil
}
