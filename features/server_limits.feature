Feature: Transient refusals keep browsers reconnecting
  As a browser client using EventSource
  I want transient server-side refusals to tell me when to retry
  So that my EventSource keeps reconnecting instead of giving up for good

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>" and at most 1 consumer
    And I am connected to SSE endpoint "/events?topic=limited"

  Scenario: A browser over the stream's consumer limit is told to retry
    When I request SSE endpoint "/events?topic=limited" as an EventSource
    Then I should receive HTTP status 200
    And the response header "Content-Type" should be "text/event-stream"
    And the response should contain ": Stream consumer limit reached"
    And the response should contain "retry: "

  Scenario: Other clients over the stream's consumer limit get 503
    When I request SSE endpoint "/events?topic=limited"
    Then I should receive HTTP status 503
    And the response should contain "Stream consumer limit reached"
