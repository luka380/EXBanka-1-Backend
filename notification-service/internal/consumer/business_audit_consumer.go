package consumer

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	kafkamsg "github.com/exbanka/contract/kafka"
	"github.com/exbanka/notification-service/internal/model"
	kafkago "github.com/segmentio/kafka-go"
	"gorm.io/gorm"
)

// BusinessAuditConsumer subscribes to the admin.business-action Kafka topic and
// persists each event as a BusinessAuditLog row. It is the authoritative audit
// trail for high-value business actions (limit changes, usedLimit resets, order
// approve/reject, permission changes, manual tax collection).
type BusinessAuditConsumer struct {
	reader *kafkago.Reader
	db     *gorm.DB
}

// NewBusinessAuditConsumer constructs a BusinessAuditConsumer and configures the
// Kafka reader. The caller must call Start(ctx) to begin consuming.
func NewBusinessAuditConsumer(brokers string, db *gorm.DB) *BusinessAuditConsumer {
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  strings.Split(brokers, ","),
		Topic:    kafkamsg.TopicBusinessAuditAction,
		GroupID:  "notification-service-business-audit",
		MinBytes: 1,
		MaxBytes: 10e6,
	})
	return &BusinessAuditConsumer{reader: reader, db: db}
}

// Start launches the consumer loop in a goroutine. It reads until ctx is
// cancelled, logging and continuing on transient errors.
func (c *BusinessAuditConsumer) Start(ctx context.Context) {
	go func() {
		log.Println("business audit consumer started, listening on", kafkamsg.TopicBusinessAuditAction)
		for {
			msg, err := c.reader.ReadMessage(ctx)
			if err != nil {
				if ctx.Err() != nil {
					log.Println("business audit consumer shutting down")
					return
				}
				log.Printf("business audit consumer: read error: %v", err)
				continue
			}
			c.handleMessage(msg.Value)
		}
	}()
}

func (c *BusinessAuditConsumer) handleMessage(data []byte) {
	var event kafkamsg.BusinessAuditActionMessage
	if err := json.Unmarshal(data, &event); err != nil {
		log.Printf("business audit consumer: unmarshal error: %v", err)
		return
	}

	row := &model.BusinessAuditLog{
		Action:     event.Action,
		ActorID:    event.ActorEmployeeID,
		TargetType: event.TargetType,
		TargetID:   event.TargetID,
		Detail:     event.Detail,
		Timestamp:  event.Timestamp,
	}
	if err := c.db.Create(row).Error; err != nil {
		log.Printf("business audit consumer: db insert error (action=%s actor=%d target=%s/%s): %v",
			event.Action, event.ActorEmployeeID, event.TargetType, event.TargetID, err)
	}
}

// Close releases the Kafka reader.
func (c *BusinessAuditConsumer) Close() error {
	return c.reader.Close()
}
