// nuts-client.js: a small EventSource helper for NUTS streams.
//
// A browser's EventSource already reconnects on its own and sends the last
// event ID it saw. What it does not do is keep that ID across a page reload,
// or start again once it gave up for good. This helper keeps the cursor in a
// storage and resumes from it:
//
//   import { subscribe } from './nuts-client.js';
//
//   const stream = subscribe('/events', {
//     topics: ['orders', 'invoices'],
//     on: {
//       message(data) { console.log(data.topic, data.payload); },
//     },
//   });
//   // later: stream.close();
//
// It takes the cursor from every event that carries one: the connected
// handshake, messages, and the reset event NUTS sends when the stream was
// recreated or restored, which sets it back to 0. With event_type topic or
// header, name every event the route sends in `on`: the ID of an event
// without a listener is not seen, so a reload resumes from an earlier one and
// repeats some events. It never skips any.
//
// No dependencies; it runs wherever there is an EventSource. Copy it into
// your application.

/**
 * Subscribes to a NUTS SSE route.
 *
 * @param {string} url The route, for example "/events". A query it already
 *   has is kept; the helper adds last-id itself.
 * @param {object} [options]
 * @param {string[]} [options.topics] Topics, sent as ?topic=… each.
 * @param {{getItem(key: string): (string|null), setItem(key: string, value: string): void}|null} [options.storage]
 *   Where the cursor survives page reloads. The default, sessionStorage,
 *   keeps one per tab. localStorage shares one between the tabs of a site:
 *   a reloaded tab resumes where another one is, and misses what arrived in
 *   between. null keeps it in memory only.
 * @param {string} [options.key] The storage key; by default "nuts:" and the
 *   URL with its topics.
 * @param {Object<string, function(*, MessageEvent): void>} [options.on]
 *   Listeners by event name. "message" is the default name; "connected" and
 *   "reset" are NUTS' own events.
 * @param {boolean} [options.raw] Pass message data as it is, for
 *   payload_format raw, instead of parsing it as JSON.
 * @param {boolean} [options.withCredentials] Send cookies cross-origin, for
 *   subscriber_jwt_cookie.
 * @param {number} [options.restartDelay] Milliseconds to wait before a new
 *   EventSource once one gave up for good (default 10000). NUTS answers
 *   passing failures in a way EventSource retries by itself; it gives up only
 *   on answers that will not change, or when a proxy in front of NUTS fails.
 * @param {function(string): void} [options.onState] Told "connecting",
 *   "open" (NUTS accepted the subscription), "retrying" (EventSource
 *   reconnects by itself), "restarting" and "closed".
 * @param {typeof EventSource} [options.EventSource] An EventSource
 *   implementation, for tests or clients outside browsers.
 * @returns {{close(): void, readonly lastEventId: string}}
 */
export function subscribe(url, options = {}) {
  const {
    topics = [],
    storage = sessionStorageOrNull(),
    on = {},
    raw = false,
    withCredentials = false,
    restartDelay = 10000,
    onState = () => {},
    EventSource: EventSourceImpl = globalThis.EventSource,
  } = options;
  const base = withQuery(url, topics.map((topic) => 'topic=' + encodeURIComponent(topic)));
  const key = options.key ?? 'nuts:' + base;

  let cursor = load();
  let source = null;
  let timer = null;
  let closed = false;

  function load() {
    try {
      return storage?.getItem(key) || '';
    } catch {
      return ''; // storage disabled: keep the cursor in memory
    }
  }

  function remember(event) {
    if (!event.lastEventId) {
      return;
    }
    cursor = event.lastEventId;
    try {
      storage?.setItem(key, cursor);
    } catch {
      // storage disabled or full: keep the cursor in memory
    }
  }

  // parse decodes an event's data. NUTS' own events are always JSON;
  // messages are unless payload_format is raw.
  function parse(name, data) {
    if (raw && name !== 'connected' && name !== 'reset') {
      return data;
    }
    try {
      return JSON.parse(data);
    } catch {
      return data;
    }
  }

  function start() {
    onState('connecting');
    // EventSource keeps this URL on its own reconnects, but NUTS prefers the
    // fresher Last-Event-ID header it sends then.
    const target = cursor ? withQuery(base, ['last-id=' + encodeURIComponent(cursor)]) : base;
    const es = new EventSourceImpl(target, { withCredentials });
    source = es;
    for (const name of new Set(['connected', 'reset', 'message', ...Object.keys(on)])) {
      es.addEventListener(name, (event) => {
        remember(event);
        if (name === 'connected') {
          onState('open');
        }
        const listener = on[name];
        if (listener) {
          listener(parse(name, event.data), event);
        }
      });
    }
    es.onerror = () => {
      if (closed || source !== es) {
        return;
      }
      if (es.readyState === EventSourceImpl.CLOSED) {
        onState('restarting');
        timer = setTimeout(start, restartDelay);
      } else {
        onState('retrying');
      }
    };
  }

  start();
  return {
    close() {
      if (closed) {
        return;
      }
      closed = true;
      clearTimeout(timer);
      timer = null;
      source.close();
      onState('closed');
    },
    get lastEventId() {
      return cursor;
    },
  };
}

function withQuery(url, params) {
  if (params.length === 0) {
    return url;
  }
  return url + (url.includes('?') ? '&' : '?') + params.join('&');
}

// sessionStorageOrNull returns the page's sessionStorage, or null where there
// is none or reading it throws (storage disabled, sandboxed frames).
function sessionStorageOrNull() {
  try {
    return globalThis.sessionStorage ?? null;
  } catch {
    return null;
  }
}
