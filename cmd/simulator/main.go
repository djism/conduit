package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	kafkago "github.com/segmentio/kafka-go"
)

// Simulator publishes realistic fake events to Kafka topics
// at a configurable rate. Every 20th event is intentionally
// malformed to demonstrate DLQ behaviour in the demo.
//
// Run with:
//   go run ./cmd/simulator
//
// Environment variables:
//   KAFKA_BROKERS  — comma-separated broker list (default: localhost:9092)
//   EVENTS_PER_SEC — publish rate per topic (default: 10)

func main() {
	broker := getEnv("KAFKA_BROKERS", "localhost:9092")
	rate := 10 // events per second per topic

	topics := []string{"orders", "payments", "notifications"}

	writers := make(map[string]*kafkago.Writer)
	for _, topic := range topics {
		writers[topic] = &kafkago.Writer{
			Addr:         kafkago.TCP(broker),
			Topic:        topic,
			Balancer:     &kafkago.LeastBytes{},
			RequiredAcks: kafkago.RequireOne,
			Async:        true,
		}
	}

	defer func() {
		for _, w := range writers {
			w.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	log.Printf("simulator started — publishing %d events/sec to %v", rate, topics)
	log.Printf("every 20th event is malformed → watch the DLQ")
	log.Println("press Ctrl+C to stop")

	counter := 0

	for {
		select {
		case <-ticker.C:
			counter++
			topic := topics[counter%len(topics)]

			var payload []byte
			var err error

			// Every 20th event is intentionally malformed
			// to demonstrate schema validation and DLQ routing.
			if counter%20 == 0 {
				payload, err = malformedEvent(topic)
				log.Printf("→ publishing MALFORMED event to %s (will hit DLQ)", topic)
			} else {
				payload, err = validEvent(topic)
			}

			if err != nil {
				log.Printf("error generating event: %v", err)
				continue
			}

			msg := kafkago.Message{
				Key:   []byte(uuid.New().String()),
				Value: payload,
				Headers: []kafkago.Header{
					{Key: "simulator", Value: []byte("true")},
					{Key: "counter", Value: []byte(fmt.Sprintf("%d", counter))},
				},
			}

			if err := writers[topic].WriteMessages(ctx, msg); err != nil {
				log.Printf("error publishing to %s: %v", topic, err)
			}

		case <-stop:
			log.Println("simulator stopped")
			return
		}
	}
}

// validEvent generates a realistic event payload for each topic.
func validEvent(topic string) ([]byte, error) {
	var event interface{}

	switch topic {
	case "orders":
		event = map[string]interface{}{
			"order_id":    uuid.New().String(),
			"customer_id": uuid.New().String(),
			"amount":      randomAmount(),
			"currency":    randomChoice([]string{"USD", "EUR", "GBP"}),
			"status":      randomChoice([]string{"pending", "confirmed", "shipped"}),
			"items":       randomInt(1, 10),
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		}

	case "payments":
		event = map[string]interface{}{
			"payment_id":   uuid.New().String(),
			"order_id":     uuid.New().String(),
			"amount":       randomAmount(),
			"currency":     randomChoice([]string{"USD", "EUR", "GBP"}),
			"method":       randomChoice([]string{"card", "paypal", "bank_transfer"}),
			"status":       randomChoice([]string{"pending", "completed", "failed"}),
			"processed_at": time.Now().UTC().Format(time.RFC3339),
		}

	case "notifications":
		event = map[string]interface{}{
			"notification_id": uuid.New().String(),
			"user_id":         uuid.New().String(),
			"type":            randomChoice([]string{"email", "sms", "push"}),
			"template":        randomChoice([]string{"order_confirmed", "shipped", "delivered"}),
			"sent_at":         time.Now().UTC().Format(time.RFC3339),
		}

	default:
		event = map[string]interface{}{
			"id":         uuid.New().String(),
			"topic":      topic,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
	}

	return json.Marshal(event)
}

// malformedEvent generates an invalid payload that will
// fail schema validation and land in the DLQ.
// Used to demonstrate the schema registry catching bad events.
func malformedEvent(topic string) ([]byte, error) {
	// Missing required fields + wrong types
	event := map[string]interface{}{
		"bad_field":  "this_should_not_be_here",
		"amount":     "not-a-number", // wrong type
		"created_at": 12345,          // wrong type
	}
	return json.Marshal(event)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func randomAmount() float64 {
	return float64(randomInt(100, 100000)) / 100.0
}

func randomInt(min, max int) int {
	return min + rand.Intn(max-min+1)
}

func randomChoice(options []string) string {
	return options[rand.Intn(len(options))]
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
