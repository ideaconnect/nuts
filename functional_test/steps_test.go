package functional_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"
	"github.com/nats-io/nats.go"
)

// clientContext holds per-client SSE state for multi-client scenarios
type clientContext struct {
	sseResponse             *http.Response
	sseEvents               []sseEvent
	allEvents               []sseEvent // accumulated across disconnect/reconnect cycles
	mu                      sync.Mutex
	cancelFunc              context.CancelFunc
	lastEventID             string
	lastEventIDAtDisconnect string // snapshot taken at disconnect time
	readDone                chan struct{}
}

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
	notedMetrics   map[string]float64
	heartbeats     []string
	clients        map[string]*clientContext
	streamNames    map[string]struct{}
	sseReadDone    chan struct{}
	natsPaused     bool
}

type sseEvent struct {
	ID    string
	Event string
	Data  string
}

var tc *testContext

const (
	functionalWaitTimeout     = 10 * time.Second
	functionalPollInterval    = 50 * time.Millisecond
	functionalQuietWindow     = 500 * time.Millisecond
	functionalDisconnectLimit = 2 * time.Second
)

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func waitUntil(description string, timeout time.Duration, check func() (bool, string)) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(functionalPollInterval)
	defer ticker.Stop()

	var detail string
	for {
		ok, currentDetail := check()
		if ok {
			return nil
		}
		if currentDetail != "" {
			detail = currentDetail
		}
		select {
		case <-deadline.C:
			if detail != "" {
				return fmt.Errorf("timed out waiting for %s: %s", description, detail)
			}
			return fmt.Errorf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func waitForNoEvent(description string, quietWindow time.Duration, check func() (bool, string)) error {
	deadline := time.NewTimer(quietWindow)
	defer deadline.Stop()
	ticker := time.NewTicker(functionalPollInterval)
	defer ticker.Stop()

	for {
		found, detail := check()
		if found {
			if detail != "" {
				return fmt.Errorf("unexpected %s: %s", description, detail)
			}
			return fmt.Errorf("unexpected %s", description)
		}
		select {
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

func waitForReadDone(done <-chan struct{}, timeout time.Duration) error {
	if done == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("SSE reader did not stop within %s", timeout)
	}
}

func singleEventsSnapshot() ([]sseEvent, []string) {
	tc.sseEventsMutex.Lock()
	defer tc.sseEventsMutex.Unlock()
	events := append([]sseEvent(nil), tc.sseEvents...)
	heartbeats := append([]string(nil), tc.heartbeats...)
	return events, heartbeats
}

func clientEventsSnapshot(cc *clientContext, includeAll bool) []sseEvent {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if !includeAll {
		return append([]sseEvent(nil), cc.sseEvents...)
	}
	events := append([]sseEvent(nil), cc.allEvents...)
	events = append(events, cc.sseEvents...)
	return events
}

func eventContains(events []sseEvent, text string) bool {
	for _, event := range events {
		if strings.Contains(event.Data, text) {
			return true
		}
	}
	return false
}

func eventHasTopic(event sseEvent, topic string) bool {
	if event.Event != "message" {
		return false
	}
	return strings.Contains(event.Data, fmt.Sprintf(`"topic":"%s"`, topic)) ||
		strings.Contains(event.Data, fmt.Sprintf(`"topic": "%s"`, topic))
}

func waitForSingleEvent(description string, match func(sseEvent) bool) error {
	return waitUntil(description, functionalWaitTimeout, func() (bool, string) {
		events, _ := singleEventsSnapshot()
		for _, event := range events {
			if match(event) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("events=%+v", events)
	})
}

func waitForSingleConnectedEvent() error {
	return waitForSingleEvent("connected SSE event", func(event sseEvent) bool {
		return event.Event == "connected"
	})
}

func splitSubjects(subjectsCSV string) []string {
	rawSubjects := strings.Split(subjectsCSV, ",")
	subjects := make([]string, 0, len(rawSubjects))
	for _, subject := range rawSubjects {
		subject = strings.TrimSpace(subject)
		if subject != "" {
			subjects = append(subjects, subject)
		}
	}
	return subjects
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	wantSet := make(map[string]int, len(want))
	for _, value := range want {
		wantSet[value]++
	}
	for _, value := range got {
		if wantSet[value] == 0 {
			return false
		}
		wantSet[value]--
	}
	return true
}

// consumerUsesExpectedMultiTopicStrategy reports whether a consumer filters
// exactly the requested subjects server-side (nats-server >= 2.10).
func consumerUsesExpectedMultiTopicStrategy(info *nats.ConsumerInfo, subjects []string) bool {
	return info.Config.FilterSubject == "" && sameStringSet(info.Config.FilterSubjects, subjects)
}

func consumerFilterSummary(infos []*nats.ConsumerInfo) string {
	parts := make([]string, 0, len(infos))
	for _, info := range infos {
		parts = append(parts, fmt.Sprintf("name=%s filter_subject=%q filter_subjects=%v", info.Name, info.Config.FilterSubject, info.Config.FilterSubjects))
	}
	return strings.Join(parts, "; ")
}

func streamShouldHaveActiveConsumerUsingExpectedMultiTopicFilters(streamName, subjectsCSV string) error {
	subjects := splitSubjects(subjectsCSV)
	if len(subjects) < 2 {
		return fmt.Errorf("expected at least two subjects, got %q", subjectsCSV)
	}
	return waitUntil("active multi-topic consumer with server-side FilterSubjects", functionalWaitTimeout, func() (bool, string) {
		var infos []*nats.ConsumerInfo
		for info := range tc.js.ConsumersInfo(streamName) {
			if info != nil {
				infos = append(infos, info)
			}
		}
		for _, info := range infos {
			if consumerUsesExpectedMultiTopicStrategy(info, subjects) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("expected FilterSubjects=%v consumers=[%s]", subjects, consumerFilterSummary(infos))
	})
}

func waitForClientConnectedEvent(name string, cc *clientContext) error {
	return waitUntil("client "+name+" connected SSE event", functionalWaitTimeout, func() (bool, string) {
		events := clientEventsSnapshot(cc, false)
		for _, event := range events {
			if event.Event == "connected" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("events=%+v", events)
	})
}

func waitForStreamAvailable(streamName string, subjects []string) error {
	return waitUntil("JetStream stream "+streamName, functionalWaitTimeout, func() (bool, string) {
		info, err := tc.js.StreamInfo(streamName)
		if err != nil {
			return false, err.Error()
		}
		for _, want := range subjects {
			found := false
			for _, got := range info.Config.Subjects {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				return false, fmt.Sprintf("subjects=%v", info.Config.Subjects)
			}
		}
		return true, ""
	})
}

func isStreamNotFound(err error) bool {
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == nats.JSErrCodeStreamNotFound
}

func deleteStreamIfExists(streamName string) error {
	if tc.js == nil {
		return nil
	}
	return waitUntil("delete JetStream stream "+streamName, functionalWaitTimeout, func() (bool, string) {
		err := tc.js.DeleteStream(streamName)
		if err == nil || isStreamNotFound(err) {
			return true, ""
		}
		return false, err.Error()
	})
}

func aNATSJetStreamServerIsRunning() error {
	return waitUntil("NATS JetStream connection", functionalWaitTimeout, func() (bool, string) {
		nc, err := nats.Connect(tc.natsURL)
		if err != nil {
			return false, fmt.Sprintf("failed to connect to NATS at %s: %v", tc.natsURL, err)
		}

		js, err := nc.JetStream()
		if err != nil {
			nc.Close()
			return false, fmt.Sprintf("failed to get JetStream context: %v", err)
		}

		tc.natsConn = nc
		tc.js = js
		return true, ""
	})
}

func theStreamExistsWithSubjects(streamName, subjects string) error {
	return createStream(streamName, subjects, 0)
}

func theStreamExistsWithSubjectsAndAtMostConsumers(streamName, subjects string, maxConsumers int) error {
	return createStream(streamName, subjects, maxConsumers)
}

// theStreamExistsOnDiskWithSubjects recreates the stream with file storage,
// for scenarios whose messages must outlive a NATS restart.
func theStreamExistsOnDiskWithSubjects(streamName, subjects string) error {
	return createStoredStream(streamName, subjects, 0, nats.FileStorage)
}

// createStream recreates the stream from scratch, in memory. maxConsumers 0
// leaves the server's default consumer limit in place.
func createStream(streamName, subjects string, maxConsumers int) error {
	return createStoredStream(streamName, subjects, maxConsumers, nats.MemoryStorage)
}

func createStoredStream(streamName, subjects string, maxConsumers int, storage nats.StorageType) error {
	if err := deleteStreamIfExists(streamName); err != nil {
		return err
	}

	_, err := tc.js.AddStream(&nats.StreamConfig{
		Name:         streamName,
		Subjects:     []string{subjects},
		Storage:      storage,
		MaxMsgs:      10000,
		MaxConsumers: maxConsumers,
	})
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}
	if tc.streamNames == nil {
		tc.streamNames = make(map[string]struct{})
	}
	tc.streamNames[streamName] = struct{}{}
	return waitForStreamAvailable(streamName, []string{subjects})
}

func iAmConnectedToSSEEndpoint(endpoint string) error {
	return iConnectToSSEEndpoint(endpoint)
}

func iConnectToSSEEndpoint(endpoint string) error {
	return connectToSSEEndpoint(endpoint, "")
}

// connectToSSEEndpoint opens the scenario's SSE stream, sending lastEventID as
// the Last-Event-ID header when it is not empty.
func connectToSSEEndpoint(endpoint, lastEventID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	tc.cancelFunc = cancel

	req, err := http.NewRequestWithContext(ctx, "GET", tc.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

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
	tc.sseReadDone = make(chan struct{})

	go readSSEEvents(resp.Body, tc.sseReadDone)

	return waitForSingleConnectedEvent()
}

func iConnectToSSEEndpointWithLastIdFromMessage(endpoint string, messageIndex int) error {
	seq, ok := tc.publishedSeqs[messageIndex]
	if !ok {
		return fmt.Errorf("no message published at index %d", messageIndex)
	}

	fullEndpoint := fmt.Sprintf("%s&last-id=%d", endpoint, seq)
	return iConnectToSSEEndpoint(fullEndpoint)
}

func iConnectToSSEEndpointWithLastEventIDFromMessage(endpoint string, messageIndex int) error {
	seq, ok := tc.publishedSeqs[messageIndex]
	if !ok {
		return fmt.Errorf("no message published at index %d", messageIndex)
	}
	return connectToSSEEndpoint(endpoint, strconv.FormatUint(seq, 10))
}

func readSSEEvents(body io.Reader, done chan<- struct{}) {
	defer close(done)
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

// iDeleteTheActiveJetStreamConsumer deletes the consumer NUTS created for
// the scenario's SSE client, simulating one reaped or lost on the server.
// NUTS' ordered consumer notices the missing heartbeats (nats_idle_heartbeat
// is 1s in Caddyfile.test) and recreates itself from the last delivered
// sequence.
func iDeleteTheActiveJetStreamConsumer(stream string) error {
	if tc.js == nil {
		return fmt.Errorf("no JetStream context — was the Background step skipped?")
	}
	// There is exactly one consumer on the test stream per scenario
	// because the SSE client only opens one subscription. ConsumersInfo
	// streams over a channel; collect everything before deleting so we
	// can fail with a clean error if there are zero or multiple.
	var names []string
	for info := range tc.js.ConsumersInfo(stream) {
		if info != nil {
			names = append(names, info.Name)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("no consumer present on stream %q — was the SSE client connected?", stream)
	}
	if len(names) > 1 {
		return fmt.Errorf("expected exactly one consumer on stream %q, got %d: %v", stream, len(names), names)
	}
	if err := tc.js.DeleteConsumer(stream, names[0]); err != nil {
		return fmt.Errorf("DeleteConsumer(%s, %s): %w", stream, names[0], err)
	}
	return nil
}

// theSSEStreamShouldStillBeOpen asserts the SSE response is still being
// streamed: the read goroutine has not returned.
func theSSEStreamShouldStillBeOpen() error {
	if tc.sseReadDone == nil {
		return fmt.Errorf("no SSE read goroutine recorded — was the client connected via /events?")
	}
	select {
	case <-tc.sseReadDone:
		// Capture whatever the client did receive so the failure is
		// debuggable (probably a stray EOF/error closed the stream
		// early — the very Batch B-leak we want to catch).
		events, heartbeats := singleEventsSnapshot()
		return fmt.Errorf("SSE read goroutine returned — the stream closed. events=%d heartbeats=%d", len(events), len(heartbeats))
	default:
		return nil
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
	return nil
}

func iShouldReceiveAnSSEEventWithTopic(topic string) error {
	return waitForSingleEvent("SSE message with topic "+topic, func(event sseEvent) bool {
		return eventHasTopic(event, topic)
	})
}

func iShouldNotReceiveAnSSEEventWithTopic(topic string) error {
	return waitForNoEvent("SSE message with topic "+topic, functionalQuietWindow, func() (bool, string) {
		events, _ := singleEventsSnapshot()
		for _, event := range events {
			if eventHasTopic(event, topic) {
				return true, fmt.Sprintf("events=%+v", events)
			}
		}
		return false, ""
	})
}

func iShouldHaveReceivedSSEMessageEvents(expected int) error {
	if err := waitUntil(fmt.Sprintf("%d SSE message events", expected), functionalWaitTimeout, func() (bool, string) {
		events, _ := singleEventsSnapshot()
		got := countMessages(events)
		return got == expected, fmt.Sprintf("got %d message events", got)
	}); err != nil {
		return err
	}
	return waitForNoEvent(fmt.Sprintf("more than %d SSE message events", expected), functionalQuietWindow, func() (bool, string) {
		events, _ := singleEventsSnapshot()
		got := countMessages(events)
		if got > expected {
			return true, fmt.Sprintf("got %d message events", got)
		}
		return false, ""
	})
}

func theEventPayloadShouldContain(text string) error {
	return waitForSingleEvent("event payload containing "+text, func(event sseEvent) bool {
		return strings.Contains(event.Data, text)
	})
}

func theEventShouldHaveAnID() error {
	return waitForSingleEvent("message event with an ID", func(event sseEvent) bool {
		return event.Event == "message" && event.ID != ""
	})
}

// theReceivedMessageEventIDsShouldBeContiguous asserts the delivery contract:
// every message event id is exactly one more than the previous one, so the
// stream has neither a gap nor a duplicate. Only meaningful for a stream
// whose topic receives every message published in the scenario.
func theReceivedMessageEventIDsShouldBeContiguous() error {
	events, _ := singleEventsSnapshot()
	var ids []uint64
	for _, event := range events {
		if event.Event != "message" {
			continue
		}
		id, err := strconv.ParseUint(event.ID, 10, 64)
		if err != nil {
			return fmt.Errorf("message event without a numeric id: %+v", event)
		}
		ids = append(ids, id)
	}
	if len(ids) < 2 {
		return fmt.Errorf("need at least two message events to check contiguity, got %v", ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			return fmt.Errorf("message ids are not contiguous: %v", ids)
		}
	}
	return nil
}

func iShouldReceiveAnSSEEventContaining(text string) error {
	return waitForSingleEvent("SSE event containing "+text, func(event sseEvent) bool {
		return strings.Contains(event.Data, text)
	})
}

func iShouldNotReceiveAnSSEEventContaining(text string) error {
	return waitForNoEvent("SSE event containing "+text, functionalQuietWindow, func() (bool, string) {
		events, _ := singleEventsSnapshot()
		if eventContains(events, text) {
			return true, fmt.Sprintf("events=%+v", events)
		}
		return false, ""
	})
}

func iShouldReceiveAEvent(eventType string) error {
	return waitForSingleEvent(eventType+" SSE event", func(event sseEvent) bool {
		return event.Event == eventType
	})
}

func theConnectedEventShouldListTopic(topic string) error {
	return waitUntil("connected event listing topic "+topic, functionalWaitTimeout, func() (bool, string) {
		events, _ := singleEventsSnapshot()
		for _, event := range events {
			if event.Event != "connected" {
				continue
			}
			var data struct {
				Topics []string `json:"topics"`
			}
			if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
				return false, fmt.Sprintf("failed to parse connected event data: %v", err)
			}
			for _, t := range data.Topics {
				if t == topic {
					return true, ""
				}
			}
			return false, fmt.Sprintf("topic %q not in connected event topics: %v", topic, data.Topics)
		}
		return false, fmt.Sprintf("events=%+v", events)
	})
}

func iRequestSSEEndpoint(endpoint string) error {
	return requestEndpoint(endpoint, "")
}

func iRequestSSEEndpointAsAnEventSource(endpoint string) error {
	return requestEndpoint(endpoint, "text/event-stream")
}

// requestEndpoint sends a GET and reads the whole response, which must end
// within 5 seconds. accept, when set, is sent as the Accept header, the way
// a browser EventSource does.
func requestEndpoint(endpoint, accept string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", tc.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	tc.httpResponse = resp

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = nil
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

func iShouldReceiveAHeartbeatComment() error {
	return waitUntil("heartbeat comment", functionalWaitTimeout, func() (bool, string) {
		_, heartbeats := singleEventsSnapshot()
		if len(heartbeats) > 0 {
			return true, ""
		}
		return false, "no heartbeat comments observed"
	})
}

// --- Multi-client step implementations ---

func readClientSSEEvents(cc *clientContext, body io.Reader) {
	defer close(cc.readDone)
	scanner := bufio.NewScanner(body)
	var currentEvent sseEvent
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if currentEvent.Event != "" || len(dataLines) > 0 {
				currentEvent.Data = strings.Join(dataLines, "\n")
				cc.mu.Lock()
				cc.sseEvents = append(cc.sseEvents, currentEvent)
				if currentEvent.ID != "" {
					cc.lastEventID = currentEvent.ID
				}
				cc.mu.Unlock()
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
		}
	}
}

func getOrCreateClient(name string) *clientContext {
	if tc.clients == nil {
		tc.clients = make(map[string]*clientContext)
	}
	cc, ok := tc.clients[name]
	if !ok {
		cc = &clientContext{}
		tc.clients[name] = cc
	}
	return cc
}

func clientIsConnectedToSSEEndpoint(name, endpoint string) error {
	cc := getOrCreateClient(name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cc.cancelFunc = cancel

	req, err := http.NewRequestWithContext(ctx, "GET", tc.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	httpClient := &http.Client{Timeout: 0}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client %q failed to connect: %w", name, err)
	}
	cc.sseResponse = resp
	cc.sseEvents = nil
	cc.readDone = make(chan struct{})

	go readClientSSEEvents(cc, resp.Body)

	return waitForClientConnectedEvent(name, cc)
}

func clientDisconnects(name string) error {
	cc, ok := tc.clients[name]
	if !ok {
		return fmt.Errorf("client %q not found", name)
	}

	// Snapshot current events into allEvents before disconnecting
	cc.mu.Lock()
	cc.allEvents = append(cc.allEvents, cc.sseEvents...)
	cc.lastEventIDAtDisconnect = cc.lastEventID
	cc.mu.Unlock()

	if cc.cancelFunc != nil {
		cc.cancelFunc()
		cc.cancelFunc = nil
	}
	if cc.sseResponse != nil {
		cc.sseResponse.Body.Close()
		cc.sseResponse = nil
	}

	return waitForReadDone(cc.readDone, functionalDisconnectLimit)
}

func clientReconnectsWithLastEventID(name, endpoint string) error {
	cc, ok := tc.clients[name]
	if !ok {
		return fmt.Errorf("client %q not found", name)
	}
	if cc.lastEventID == "" {
		return fmt.Errorf("client %q has no last event ID", name)
	}

	sep := "&"
	if !strings.Contains(endpoint, "?") {
		sep = "?"
	}
	fullEndpoint := fmt.Sprintf("%s%slast-id=%s", endpoint, sep, cc.lastEventID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cc.cancelFunc = cancel

	req, err := http.NewRequestWithContext(ctx, "GET", tc.baseURL+fullEndpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	httpClient := &http.Client{Timeout: 0}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client %q failed to reconnect: %w", name, err)
	}
	cc.sseResponse = resp
	cc.sseEvents = nil
	cc.readDone = make(chan struct{})

	go readClientSSEEvents(cc, resp.Body)

	return waitForClientConnectedEvent(name, cc)
}

func clientConnectsWithLastEventIDFromClient(name, endpoint, otherName string) error {
	other, ok := tc.clients[otherName]
	if !ok {
		return fmt.Errorf("client %q not found", otherName)
	}

	// Use the snapshot taken at disconnect time so we are not affected
	// by events the other client received after reconnecting.
	other.mu.Lock()
	lastID := other.lastEventIDAtDisconnect
	if lastID == "" {
		lastID = other.lastEventID
	}
	other.mu.Unlock()

	if lastID == "" {
		return fmt.Errorf("client %q has no last event ID", otherName)
	}

	cc := getOrCreateClient(name)
	cc.lastEventID = lastID

	sep := "&"
	if !strings.Contains(endpoint, "?") {
		sep = "?"
	}
	fullEndpoint := fmt.Sprintf("%s%slast-id=%s", endpoint, sep, cc.lastEventID)

	return clientIsConnectedToSSEEndpoint(name, fullEndpoint)
}

func countMessages(events []sseEvent) int {
	n := 0
	for _, e := range events {
		if e.Event == "message" {
			n++
		}
	}
	return n
}

func clientShouldHaveReceivedNMessages(name string, expected int) error {
	cc, ok := tc.clients[name]
	if !ok {
		return fmt.Errorf("client %q not found", name)
	}

	if err := waitUntil(fmt.Sprintf("client %q to receive %d messages", name, expected), functionalWaitTimeout, func() (bool, string) {
		got := countMessages(clientEventsSnapshot(cc, false))
		if got >= expected {
			if got != expected {
				return false, fmt.Sprintf("expected exactly %d messages, got %d", expected, got)
			}
			return true, ""
		}
		return false, fmt.Sprintf("got %d messages", got)
	}); err != nil {
		return err
	}
	// A late duplicate would only show up after the count was reached.
	return waitForNoEvent(fmt.Sprintf("client %q receiving more than %d messages", name, expected), functionalQuietWindow, func() (bool, string) {
		if got := countMessages(clientEventsSnapshot(cc, false)); got > expected {
			return true, fmt.Sprintf("got %d messages", got)
		}
		return false, ""
	})
}

func clientShouldHaveReceivedNMessagesInTotal(name string, expected int) error {
	cc, ok := tc.clients[name]
	if !ok {
		return fmt.Errorf("client %q not found", name)
	}

	if err := waitUntil(fmt.Sprintf("client %q to receive %d total messages", name, expected), functionalWaitTimeout, func() (bool, string) {
		got := countMessages(clientEventsSnapshot(cc, true))
		if got >= expected {
			if got != expected {
				return false, fmt.Sprintf("expected exactly %d total messages, got %d", expected, got)
			}
			return true, ""
		}
		return false, fmt.Sprintf("got %d total messages", got)
	}); err != nil {
		return err
	}
	return waitForNoEvent(fmt.Sprintf("client %q receiving more than %d total messages", name, expected), functionalQuietWindow, func() (bool, string) {
		if got := countMessages(clientEventsSnapshot(cc, true)); got > expected {
			return true, fmt.Sprintf("got %d total messages", got)
		}
		return false, ""
	})
}

func clientShouldHaveReceivedEventContaining(name, text string) error {
	cc, ok := tc.clients[name]
	if !ok {
		return fmt.Errorf("client %q not found", name)
	}

	return waitUntil(fmt.Sprintf("client %q event containing %s", name, text), functionalWaitTimeout, func() (bool, string) {
		events := clientEventsSnapshot(cc, true)
		if eventContains(events, text) {
			return true, ""
		}
		return false, fmt.Sprintf("events=%+v", events)
	})
}

func clientShouldNotHaveReceivedEventContaining(name, text string) error {
	cc, ok := tc.clients[name]
	if !ok {
		return fmt.Errorf("client %q not found", name)
	}

	return waitForNoEvent(fmt.Sprintf("client %q event containing %s", name, text), functionalQuietWindow, func() (bool, string) {
		events := clientEventsSnapshot(cc, true)
		if eventContains(events, text) {
			return true, fmt.Sprintf("events=%+v", events)
		}
		return false, ""
	})
}

func cleanupScenarioState() error {
	var cleanupErrs []string

	// A scenario that failed while NATS was paused must not leave it paused
	// for the ones after it.
	if tc.natsPaused {
		if err := natsAnswersAgain(); err != nil {
			cleanupErrs = append(cleanupErrs, err.Error())
		}
	}

	if tc.cancelFunc != nil {
		tc.cancelFunc()
		tc.cancelFunc = nil
	}
	if tc.sseResponse != nil {
		if err := tc.sseResponse.Body.Close(); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("close single-client SSE body: %v", err))
		}
		tc.sseResponse = nil
	}
	if err := waitForReadDone(tc.sseReadDone, functionalDisconnectLimit); err != nil {
		cleanupErrs = append(cleanupErrs, err.Error())
	}
	tc.sseReadDone = nil
	if tc.httpResponse != nil && tc.httpResponse.Body != nil {
		if err := tc.httpResponse.Body.Close(); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("close HTTP response body: %v", err))
		}
		tc.httpResponse = nil
	}

	for name, cc := range tc.clients {
		if cc.cancelFunc != nil {
			cc.cancelFunc()
			cc.cancelFunc = nil
		}
		if cc.sseResponse != nil {
			if err := cc.sseResponse.Body.Close(); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Sprintf("close client %q SSE body: %v", name, err))
			}
			cc.sseResponse = nil
		}
		if err := waitForReadDone(cc.readDone, functionalDisconnectLimit); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("client %q: %v", name, err))
		}
	}

	if tc.js != nil {
		for streamName := range tc.streamNames {
			// NUTS deletes a stream's consumers after its client goes away.
			// Wait for that before deleting the stream: a consumer still
			// being recreated would otherwise reappear on the next
			// scenario's fresh stream.
			waitForNoConsumers(streamName, functionalDisconnectLimit)
			if err := deleteStreamIfExists(streamName); err != nil {
				cleanupErrs = append(cleanupErrs, err.Error())
			}
		}
	}
	if tc.natsConn != nil {
		tc.natsConn.Close()
		tc.natsConn = nil
	}
	tc.js = nil

	if len(cleanupErrs) > 0 {
		return errors.New(strings.Join(cleanupErrs, "; "))
	}
	return nil
}

func resetScenarioState() {
	tc.sseEvents = nil
	tc.httpResponse = nil
	tc.httpBody = ""
	tc.heartbeats = nil
	tc.publishedSeqs = make(map[int]uint64)
	tc.clients = make(map[string]*clientContext)
	tc.streamNames = make(map[string]struct{})
	tc.sseReadDone = nil
}

func InitializeScenario(ctx *godog.ScenarioContext) {
	tc = &testContext{
		baseURL:       getEnvOrDefault("TEST_BASE_URL", "http://localhost:8080"),
		natsURL:       getEnvOrDefault("TEST_NATS_URL", "nats://localhost:4222"),
		publishedSeqs: make(map[int]uint64),
		clients:       make(map[string]*clientContext),
		streamNames:   make(map[string]struct{}),
	}

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		if err := cleanupScenarioState(); err != nil {
			return ctx, err
		}
		resetScenarioState()
		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		return ctx, cleanupScenarioState()
	})

	// Background steps
	ctx.Step(`^a NATS JetStream server is running$`, aNATSJetStreamServerIsRunning)
	ctx.Step(`^the stream "([^"]*)" exists with subjects "([^"]*)"$`, theStreamExistsWithSubjects)
	ctx.Step(`^the stream "([^"]*)" exists with subjects "([^"]*)" and at most (\d+) consumers?$`, theStreamExistsWithSubjectsAndAtMostConsumers)
	ctx.Step(`^the stream "([^"]*)" exists on disk with subjects "([^"]*)"$`, theStreamExistsOnDiskWithSubjects)

	// Given steps
	ctx.Step(`^I am connected to SSE endpoint "([^"]*)"$`, iAmConnectedToSSEEndpoint)
	ctx.Step(`^I publish message '([^']*)' to subject "([^"]*)"$`, iPublishMessageToSubject)

	// When steps
	ctx.Step(`^I connect to SSE endpoint "([^"]*)"$`, iConnectToSSEEndpoint)
	ctx.Step(`^I connect to SSE endpoint "([^"]*)" with last-id from message (\d+)$`, iConnectToSSEEndpointWithLastIdFromMessage)
	ctx.Step(`^I connect to SSE endpoint "([^"]*)" with Last-Event-ID from message (\d+)$`, iConnectToSSEEndpointWithLastEventIDFromMessage)
	ctx.Step(`^I publish message '([^']*)' to subject "([^"]*)"$`, iPublishMessageToSubject)
	ctx.Step(`^I request SSE endpoint "([^"]*)"$`, iRequestSSEEndpoint)
	ctx.Step(`^I request SSE endpoint "([^"]*)" as an EventSource$`, iRequestSSEEndpointAsAnEventSource)
	ctx.Step(`^I send OPTIONS request to "([^"]*)" with origin "([^"]*)"$`, iSendOPTIONSRequestToWithOrigin)

	// Then steps
	ctx.Step(`^I should receive an SSE event with topic "([^"]*)"$`, iShouldReceiveAnSSEEventWithTopic)
	ctx.Step(`^I should not receive an SSE event with topic "([^"]*)"$`, iShouldNotReceiveAnSSEEventWithTopic)
	ctx.Step(`^I should have received (\d+) SSE message events?$`, iShouldHaveReceivedSSEMessageEvents)
	ctx.Step(`^the event payload should contain "([^"]*)"$`, theEventPayloadShouldContain)
	ctx.Step(`^the event should have an ID$`, theEventShouldHaveAnID)
	ctx.Step(`^I should receive an SSE event containing '([^']*)'$`, iShouldReceiveAnSSEEventContaining)
	ctx.Step(`^I should not receive an SSE event containing '([^']*)'$`, iShouldNotReceiveAnSSEEventContaining)
	ctx.Step(`^I should receive a "([^"]*)" event$`, iShouldReceiveAEvent)
	ctx.Step(`^the connected event should list topic "([^"]*)"$`, theConnectedEventShouldListTopic)
	ctx.Step(`^I should receive HTTP status (\d+)$`, iShouldReceiveHTTPStatus)
	ctx.Step(`^the response should contain "([^"]*)"$`, theResponseShouldContain)
	ctx.Step(`^the response header "([^"]*)" should be "([^"]*)"$`, theResponseHeaderShouldBe)
	ctx.Step(`^the SSE response header "([^"]*)" should be "([^"]*)"$`, theSSEResponseHeaderShouldBe)
	ctx.Step(`^I should receive a heartbeat comment$`, iShouldReceiveAHeartbeatComment)
	ctx.Step(`^the stream "([^"]*)" should have an active consumer using expected multi-topic filters for subjects "([^"]*)"$`, streamShouldHaveActiveConsumerUsingExpectedMultiTopicFilters)

	// M9 Batch A — consumer invalidation observability
	ctx.Step(`^I delete the active JetStream consumer for stream "([^"]*)"$`, iDeleteTheActiveJetStreamConsumer)
	ctx.Step(`^the SSE stream should still be open$`, theSSEStreamShouldStillBeOpen)
	ctx.Step(`^I note the value of metric '([^']*)'$`, iNoteTheValueOfMetric)
	ctx.Step(`^the metric '([^']*)' should have increased$`, theMetricShouldHaveIncreased)
	ctx.Step(`^the metric '([^']*)' should not have changed$`, theMetricShouldNotHaveChanged)
	ctx.Step(`^I publish (\d+) messages to subject "([^"]*)"$`, iPublishNMessagesToSubject)
	ctx.Step(`^I publish (\d+) messages to subject "([^"]*)" at once$`, iPublishNMessagesToSubjectAtOnce)
	ctx.Step(`^NATS restarts$`, natsRestarts)
	ctx.Step(`^NATS stops answering$`, natsStopsAnswering)
	ctx.Step(`^NATS answers again$`, natsAnswersAgain)
	ctx.Step(`^an EventSource request for SSE endpoint "([^"]*)" is told to retry within (\d+) seconds?$`, anEventSourceRequestIsToldToRetryWithin)
	ctx.Step(`^the stream "([^"]*)" should have (\d+) consumers? for subject "([^"]*)"$`, theStreamShouldHaveConsumersForSubject)
	ctx.Step(`^the received message event ids should be contiguous$`, theReceivedMessageEventIDsShouldBeContiguous)

	// Multi-client steps
	ctx.Step(`^client "([^"]*)" is connected to SSE endpoint "([^"]*)"$`, clientIsConnectedToSSEEndpoint)
	ctx.Step(`^client "([^"]*)" should have received (\d+) messages$`, clientShouldHaveReceivedNMessages)
	ctx.Step(`^client "([^"]*)" disconnects$`, clientDisconnects)
	ctx.Step(`^client "([^"]*)" reconnects to SSE endpoint "([^"]*)" with its last event ID$`, clientReconnectsWithLastEventID)
	ctx.Step(`^client "([^"]*)" connects to SSE endpoint "([^"]*)" with last event ID from client "([^"]*)"$`, clientConnectsWithLastEventIDFromClient)
	ctx.Step(`^client "([^"]*)" should have received (\d+) messages in total$`, clientShouldHaveReceivedNMessagesInTotal)
	ctx.Step(`^client "([^"]*)" should have received an event containing '([^']*)'$`, clientShouldHaveReceivedEventContaining)
	ctx.Step(`^client "([^"]*)" should not have received an event containing '([^']*)'$`, clientShouldNotHaveReceivedEventContaining)
}

// theStreamShouldHaveConsumersForSubject waits until exactly want of the
// stream's consumers filter on subject; consumer deletes happen in the
// background.
func theStreamShouldHaveConsumersForSubject(streamName string, want int, subject string) error {
	return waitUntil("consumer count", functionalWaitTimeout, func() (bool, string) {
		got := 0
		for info := range tc.js.ConsumersInfo(streamName) {
			if info.Config.FilterSubject == subject || slices.Contains(info.Config.FilterSubjects, subject) {
				got++
			}
		}
		if got == want {
			return true, ""
		}
		return false, fmt.Sprintf("stream %s has %d consumers for %s, want %d", streamName, got, subject, want)
	})
}

// waitForNoConsumers waits, at most timeout, until the stream has no
// consumers left.
func waitForNoConsumers(streamName string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := tc.js.StreamInfo(streamName)
		if err != nil || info.State.Consumers == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// theSSEResponseHeaderShouldBe checks a header of the scenario's open SSE
// stream.
func theSSEResponseHeaderShouldBe(name, want string) error {
	if tc.sseResponse == nil {
		return fmt.Errorf("no SSE stream is open")
	}
	if got := tc.sseResponse.Header.Get(name); got != want {
		return fmt.Errorf("SSE response header %s = %q, want %q", name, got, want)
	}
	return nil
}

// metricValue scrapes Caddy's /metrics and returns the value of one series,
// written as it appears in the exposition format (name{labels}); a series
// that has not been incremented yet reads as 0.
func metricValue(series string) (float64, error) {
	resp, err := http.Get(tc.baseURL + "/metrics")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, series+" "); ok {
			return strconv.ParseFloat(strings.TrimSpace(value), 64)
		}
	}
	return 0, nil
}

func iNoteTheValueOfMetric(series string) error {
	value, err := metricValue(series)
	if err != nil {
		return err
	}
	if tc.notedMetrics == nil {
		tc.notedMetrics = map[string]float64{}
	}
	tc.notedMetrics[series] = value
	return nil
}

func theMetricShouldHaveIncreased(series string) error {
	before, ok := tc.notedMetrics[series]
	if !ok {
		return fmt.Errorf("metric %s was not noted", series)
	}
	return waitUntil("metric "+series+" to increase", functionalWaitTimeout, func() (bool, string) {
		value, err := metricValue(series)
		if err != nil {
			return false, err.Error()
		}
		return value > before, fmt.Sprintf("%s = %v, noted %v", series, value, before)
	})
}

// theMetricShouldNotHaveChanged compares a series with its noted value.
func theMetricShouldNotHaveChanged(series string) error {
	before, ok := tc.notedMetrics[series]
	if !ok {
		return fmt.Errorf("metric %s was not noted", series)
	}
	value, err := metricValue(series)
	if err != nil {
		return err
	}
	if value != before {
		return fmt.Errorf("%s = %v, noted %v", series, value, before)
	}
	return nil
}

// natsRestarts restarts the stack's NATS container, then waits until
// JetStream answers this suite again and NUTS reports ready. Both clients
// reconnect by themselves.
func natsRestarts() error {
	container := getEnvOrDefault("TEST_NATS_CONTAINER", "nuts-nats")
	if out, err := exec.Command("docker", "restart", container).CombinedOutput(); err != nil {
		return fmt.Errorf("docker restart %s: %v: %s", container, err, strings.TrimSpace(string(out)))
	}
	if err := waitUntil("JetStream after the NATS restart", 3*functionalWaitTimeout, func() (bool, string) {
		if _, err := tc.js.AccountInfo(); err != nil {
			return false, err.Error()
		}
		return true, ""
	}); err != nil {
		return err
	}
	return waitUntil("NUTS ready after the NATS restart", 3*functionalWaitTimeout, func() (bool, string) {
		resp, err := http.Get(tc.baseURL + "/events/readyz")
		if err != nil {
			return false, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("readyz answered %d", resp.StatusCode)
	})
}

// natsStopsAnswering pauses the stack's NATS container: its connections stay
// open but nothing is answered, like a VM that froze or a link that drops
// every packet.
func natsStopsAnswering() error {
	container := getEnvOrDefault("TEST_NATS_CONTAINER", "nuts-nats")
	if out, err := exec.Command("docker", "pause", container).CombinedOutput(); err != nil {
		return fmt.Errorf("docker pause %s: %v: %s", container, err, strings.TrimSpace(string(out)))
	}
	tc.natsPaused = true
	return nil
}

// natsAnswersAgain unpauses the stack's NATS container and waits until NUTS
// reports ready again.
func natsAnswersAgain() error {
	container := getEnvOrDefault("TEST_NATS_CONTAINER", "nuts-nats")
	if out, err := exec.Command("docker", "unpause", container).CombinedOutput(); err != nil {
		return fmt.Errorf("docker unpause %s: %v: %s", container, err, strings.TrimSpace(string(out)))
	}
	tc.natsPaused = false
	return waitUntil("NUTS ready after NATS answers again", 3*functionalWaitTimeout, func() (bool, string) {
		resp, err := http.Get(tc.baseURL + "/events/readyz")
		if err != nil {
			return false, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("readyz answered %d", resp.StatusCode)
	})
}

// anEventSourceRequestIsToldToRetryWithin keeps sending EventSource-style
// requests, each given a second, until one is answered with a retry: stream.
// Before NUTS notices an outage, requests wait out their JetStream timeouts
// and miss that second.
func anEventSourceRequestIsToldToRetryWithin(endpoint string, seconds int) error {
	client := &http.Client{Timeout: time.Second}
	return waitUntil("a retry: answer from "+endpoint, time.Duration(seconds)*time.Second, func() (bool, string) {
		req, err := http.NewRequest("GET", tc.baseURL+endpoint, nil)
		if err != nil {
			return false, err.Error()
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			return false, err.Error()
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return false, err.Error()
		}
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "retry:") {
			return true, ""
		}
		return false, fmt.Sprintf("answered %d: %q", resp.StatusCode, body)
	})
}

// iPublishNMessagesToSubjectAtOnce publishes without waiting for each ack,
// so the messages reach the stream as one burst.
func iPublishNMessagesToSubjectAtOnce(count int, subject string) error {
	for i := 1; i <= count; i++ {
		if _, err := tc.js.PublishAsync(subject, []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			return fmt.Errorf("publish %d: %w", i, err)
		}
	}
	select {
	case <-tc.js.PublishAsyncComplete():
		return nil
	case <-time.After(functionalWaitTimeout):
		return fmt.Errorf("%d async publishes not acknowledged", tc.js.PublishAsyncPending())
	}
}

func iPublishNMessagesToSubject(count int, subject string) error {
	for i := 1; i <= count; i++ {
		if err := iPublishMessageToSubject(fmt.Sprintf(`{"n":%d}`, i), subject); err != nil {
			return err
		}
	}
	return nil
}
