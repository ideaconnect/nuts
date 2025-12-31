package functional_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"
	"github.com/nats-io/nats.go"
)

// testContext holds state for each scenario
type testContext struct {
	natsConn       *nats.Conn
	js             nats.JetStreamContext
	sseResponse    *http.Response
	sseEvents      []sseEvent
	sseEventsMutex sync.Mutex
	httpResponse   *http.Response
	httpBody       string
	baseURL        string
	natsURL        string
	cancelFunc     context.CancelFunc
	publishedSeqs  map[int]uint64 // maps message index to JetStream sequence
	heartbeats     []string
}

type sseEvent struct {
	ID    string
	Event string
	Data  string
}

var tc *testContext

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func aNATSJetStreamServerIsRunning() error {
	nc, err := nats.Connect(tc.natsURL)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS at %s: %w", tc.natsURL, err)
	}
	tc.natsConn = nc

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("failed to get JetStream context: %w", err)
	}
	tc.js = js

	return nil
}

func theStreamExistsWithSubjects(streamName, subjects string) error {
	// Delete stream if exists (cleanup from previous runs)
	_ = tc.js.DeleteStream(streamName)

	_, err := tc.js.AddStream(&nats.StreamConfig{
		Name:     streamName,
		Subjects: []string{subjects},
		Storage:  nats.MemoryStorage,
		MaxMsgs:  10000,
	})
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}
	return nil
}

func iAmConnectedToSSEEndpoint(endpoint string) error {
	return iConnectToSSEEndpoint(endpoint)
}

func iConnectToSSEEndpoint(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	tc.cancelFunc = cancel

	req, err := http.NewRequestWithContext(ctx, "GET", tc.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{
		Timeout: 0, // No timeout for SSE
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to SSE endpoint: %w", err)
	}
	tc.sseResponse = resp
	tc.sseEvents = nil
	tc.heartbeats = nil

	// Start reading events in background
	go readSSEEvents(resp.Body)

	// Wait for connection to establish and receive connected event
	time.Sleep(300 * time.Millisecond)
	return nil
}

func iConnectToSSEEndpointWithLastIdFromMessage(endpoint string, messageIndex int) error {
	seq, ok := tc.publishedSeqs[messageIndex]
	if !ok {
		return fmt.Errorf("no message published at index %d", messageIndex)
	}

	fullEndpoint := fmt.Sprintf("%s&last-id=%d", endpoint, seq)
	return iConnectToSSEEndpoint(fullEndpoint)
}

func readSSEEvents(body io.Reader) {
	scanner := bufio.NewScanner(body)
	var currentEvent sseEvent
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line marks end of event
			if currentEvent.Event != "" || len(dataLines) > 0 {
				currentEvent.Data = strings.Join(dataLines, "\n")
				tc.sseEventsMutex.Lock()
				tc.sseEvents = append(tc.sseEvents, currentEvent)
				tc.sseEventsMutex.Unlock()
				currentEvent = sseEvent{}
				dataLines = nil
			}
			continue
		}

		if strings.HasPrefix(line, "id: ") {
			currentEvent.ID = strings.TrimPrefix(line, "id: ")
		} else if strings.HasPrefix(line, "event: ") {
			currentEvent.Event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		} else if strings.HasPrefix(line, ": heartbeat") {
			tc.sseEventsMutex.Lock()
			tc.heartbeats = append(tc.heartbeats, line)
			tc.sseEventsMutex.Unlock()
		}
	}
}

func iPublishMessageToSubject(message, subject string) error {
	ack, err := tc.js.Publish(subject, []byte(message))
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	// Track the sequence for replay tests
	if tc.publishedSeqs == nil {
		tc.publishedSeqs = make(map[int]uint64)
	}
	tc.publishedSeqs[len(tc.publishedSeqs)+1] = ack.Sequence

	// Wait for message to propagate
	time.Sleep(100 * time.Millisecond)
	return nil
}

func iShouldReceiveAnSSEEventWithTopic(topic string) error {
	// Wait a bit for events to arrive
	time.Sleep(500 * time.Millisecond)

	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if event.Event == "message" {
			// Check for topic in the data
			if strings.Contains(event.Data, fmt.Sprintf(`"topic":"%s"`, topic)) ||
				strings.Contains(event.Data, fmt.Sprintf(`"topic": "%s"`, topic)) {
				return nil
			}
		}
	}
	return fmt.Errorf("no SSE event found with topic %q, got events: %+v", topic, tc.sseEvents)
}

func theEventPayloadShouldContain(text string) error {
	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if strings.Contains(event.Data, text) {
			return nil
		}
	}
	return fmt.Errorf("no event payload contains %q", text)
}

func theEventShouldHaveAnID() error {
	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if event.Event == "message" && event.ID != "" {
			return nil
		}
	}
	return fmt.Errorf("no message event has an ID, events: %+v", tc.sseEvents)
}

func iShouldReceiveAnSSEEventContaining(text string) error {
	time.Sleep(500 * time.Millisecond)

	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if strings.Contains(event.Data, text) {
			return nil
		}
	}
	return fmt.Errorf("no SSE event contains %q, got: %+v", text, tc.sseEvents)
}

func iShouldNotReceiveAnSSEEventContaining(text string) error {
	time.Sleep(500 * time.Millisecond)

	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if strings.Contains(event.Data, text) {
			return fmt.Errorf("unexpected SSE event containing %q found", text)
		}
	}
	return nil
}

func iShouldReceiveAEvent(eventType string) error {
	time.Sleep(300 * time.Millisecond)

	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if event.Event == eventType {
			return nil
		}
	}
	return fmt.Errorf("no %q event received, got: %+v", eventType, tc.sseEvents)
}

func theConnectedEventShouldListTopic(topic string) error {
	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	for _, event := range tc.sseEvents {
		if event.Event == "connected" {
			var data struct {
				Topics []string `json:"topics"`
			}
			if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
				return fmt.Errorf("failed to parse connected event data: %w", err)
			}
			for _, t := range data.Topics {
				if t == topic {
					return nil
				}
			}
			return fmt.Errorf("topic %q not in connected event topics: %v", topic, data.Topics)
		}
	}
	return fmt.Errorf("no connected event found")
}

func iRequestSSEEndpoint(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", tc.baseURL+endpoint, nil)
	if err != nil {
		return err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	tc.httpResponse = resp

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	tc.httpBody = string(body)

	return nil
}

func iShouldReceiveHTTPStatus(status int) error {
	if tc.httpResponse == nil {
		return fmt.Errorf("no HTTP response received")
	}
	if tc.httpResponse.StatusCode != status {
		return fmt.Errorf("expected status %d, got %d (body: %s)", status, tc.httpResponse.StatusCode, tc.httpBody)
	}
	return nil
}

func theResponseShouldContain(text string) error {
	if !strings.Contains(tc.httpBody, text) {
		return fmt.Errorf("response does not contain %q, got: %s", text, tc.httpBody)
	}
	return nil
}

func iSendOPTIONSRequestToWithOrigin(endpoint, origin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "OPTIONS", tc.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", origin)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	tc.httpResponse = resp
	return nil
}

func theResponseHeaderShouldBe(header, value string) error {
	if tc.httpResponse == nil {
		return fmt.Errorf("no HTTP response")
	}
	actual := tc.httpResponse.Header.Get(header)
	if actual != value {
		return fmt.Errorf("header %q: expected %q, got %q", header, value, actual)
	}
	return nil
}

func iWaitForSeconds(seconds int) error {
	time.Sleep(time.Duration(seconds) * time.Second)
	return nil
}

func iShouldReceiveAHeartbeatComment() error {
	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()

	if len(tc.heartbeats) > 0 {
		return nil
	}
	return fmt.Errorf("no heartbeat comment received")
}

func InitializeScenario(ctx *godog.ScenarioContext) {
	tc = &testContext{
		baseURL:       getEnvOrDefault("TEST_BASE_URL", "http://localhost:8080"),
		natsURL:       getEnvOrDefault("TEST_NATS_URL", "nats://localhost:4222"),
		publishedSeqs: make(map[int]uint64),
	}

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		// Reset state for each scenario
		tc.sseEvents = nil
		tc.httpResponse = nil
		tc.httpBody = ""
		tc.heartbeats = nil
		tc.publishedSeqs = make(map[int]uint64)
		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		// Cleanup
		if tc.cancelFunc != nil {
			tc.cancelFunc()
		}
		if tc.sseResponse != nil {
			tc.sseResponse.Body.Close()
		}
		if tc.natsConn != nil {
			tc.natsConn.Close()
		}
		return ctx, nil
	})

	// Background steps
	ctx.Step(`^a NATS JetStream server is running$`, aNATSJetStreamServerIsRunning)
	ctx.Step(`^the stream "([^"]*)" exists with subjects "([^"]*)"$`, theStreamExistsWithSubjects)

	// Given steps
	ctx.Step(`^I am connected to SSE endpoint "([^"]*)"$`, iAmConnectedToSSEEndpoint)
	ctx.Step(`^I publish message '([^']*)' to subject "([^"]*)"$`, iPublishMessageToSubject)

	// When steps
	ctx.Step(`^I connect to SSE endpoint "([^"]*)"$`, iConnectToSSEEndpoint)
	ctx.Step(`^I connect to SSE endpoint "([^"]*)" with last-id from message (\d+)$`, iConnectToSSEEndpointWithLastIdFromMessage)
	ctx.Step(`^I publish message '([^']*)' to subject "([^"]*)"$`, iPublishMessageToSubject)
	ctx.Step(`^I request SSE endpoint "([^"]*)"$`, iRequestSSEEndpoint)
	ctx.Step(`^I send OPTIONS request to "([^"]*)" with origin "([^"]*)"$`, iSendOPTIONSRequestToWithOrigin)
	ctx.Step(`^I wait for (\d+) seconds?$`, iWaitForSeconds)

	// Then steps
	ctx.Step(`^I should receive an SSE event with topic "([^"]*)"$`, iShouldReceiveAnSSEEventWithTopic)
	ctx.Step(`^the event payload should contain "([^"]*)"$`, theEventPayloadShouldContain)
	ctx.Step(`^the event should have an ID$`, theEventShouldHaveAnID)
	ctx.Step(`^I should receive an SSE event containing '([^']*)'$`, iShouldReceiveAnSSEEventContaining)
	ctx.Step(`^I should not receive an SSE event containing '([^']*)'$`, iShouldNotReceiveAnSSEEventContaining)
	ctx.Step(`^I should receive a "([^"]*)" event$`, iShouldReceiveAEvent)
	ctx.Step(`^the connected event should list topic "([^"]*)"$`, theConnectedEventShouldListTopic)
	ctx.Step(`^I should receive HTTP status (\d+)$`, iShouldReceiveHTTPStatus)
	ctx.Step(`^the response should contain "([^"]*)"$`, theResponseShouldContain)
	ctx.Step(`^the response header "([^"]*)" should be "([^"]*)"$`, theResponseHeaderShouldBe)
	ctx.Step(`^I should receive a heartbeat comment$`, iShouldReceiveAHeartbeatComment)
}
