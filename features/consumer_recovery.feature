Feature: M9 Batch A — consumer invalidation observability
  As an operator running NUTS with default-on nats_idle_heartbeat
  I want a server-side reaped JetStream consumer to be detectable
  But I want the SSE connection to stay open until Batch B's
  termination arm lands

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  # Functional confirmation of the Batch A contract end-to-end through the
  # real Caddy + NATS docker-compose stack. The unit-integration tests in
  # handler_integration_test.go (TestHandler_BatchA_*) cover the same
  # contract against the embedded server; this scenario adds the wire-
  # level confirmation that a real HTTP SSE client — the same shape a
  # browser EventSource would use — observes the contract over the actual
  # production HTTP path through Caddy.
  #
  # Caddyfile.test sets nats_idle_heartbeat 1 so detection happens in
  # ~2-3 seconds rather than the production default of ~20s.
  Scenario: SSE stream stays open after the JetStream consumer is reaped
    # Establish a baseline: client connects, message round-trips so we
    # know the subscription is alive on both sides.
    Given I am connected to SSE endpoint "/events?topic=invalidation"
    When I publish message '{"phase":"baseline"}' to subject "events.invalidation"
    Then I should receive an SSE event containing 'baseline'

    # Force the failure: delete the server-side ephemeral consumer
    # holding the SSE subscription. nats.go's IdleHeartbeat-miss
    # detector fires nats.ErrConsumerNotActive after roughly 2× the
    # heartbeat interval (~2-3s with 1s heartbeats).
    When I delete the active JetStream consumer for stream "EVENTS"
    And I wait 3 seconds for heartbeat-miss detection

    # CRITICAL BATCH A CONTRACT: the SSE connection MUST stay open.
    # Batch B (#54) will convert the missed heartbeat into a clean
    # disconnect with disconnect_reason=consumer_invalidated; until
    # then the handler stays attached to the (now zombie) consumer.
    # A regression that prematurely added the disconnect would fail
    # this assertion before reaching production.
    Then the SSE stream should still be open
    And I should keep receiving heartbeat comments
