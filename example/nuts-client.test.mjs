// Tests for nuts-client.js, run by `make test-js` with Node's built-in test
// runner; they need no packages.
import { test, mock } from 'node:test';
import assert from 'node:assert/strict';
import { subscribe } from './nuts-client.js';

// FakeEventSource records what the helper does with it and dispatches events
// the way a browser would.
class FakeEventSource {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  static created = [];

  constructor(url, init) {
    this.url = url;
    this.init = init;
    this.readyState = FakeEventSource.CONNECTING;
    this.listeners = new Map();
    this.onerror = null;
    this.closed = false;
    FakeEventSource.created.push(this);
  }

  addEventListener(name, listener) {
    const list = this.listeners.get(name) ?? [];
    list.push(listener);
    this.listeners.set(name, list);
  }

  // emit dispatches an event with its data and, as EventSource does, the last
  // event ID so far.
  emit(name, data, lastEventId = '') {
    this.readyState = FakeEventSource.OPEN;
    for (const listener of this.listeners.get(name) ?? []) {
      listener({ type: name, data, lastEventId });
    }
  }

  fail(closed) {
    this.readyState = closed ? FakeEventSource.CLOSED : FakeEventSource.CONNECTING;
    this.onerror?.({ type: 'error' });
  }

  close() {
    this.closed = true;
    this.readyState = FakeEventSource.CLOSED;
  }
}

function memoryStorage(initial = {}) {
  const items = new Map(Object.entries(initial));
  return {
    getItem: (key) => (items.has(key) ? items.get(key) : null),
    setItem: (key, value) => items.set(key, String(value)),
  };
}

function last() {
  return FakeEventSource.created[FakeEventSource.created.length - 1];
}

// withSessionStorage runs fn with globalThis.sessionStorage defined by
// descriptor, as a browser page would have it.
function withSessionStorage(descriptor, fn) {
  const saved = Object.getOwnPropertyDescriptor(globalThis, 'sessionStorage');
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, ...descriptor });
  try {
    fn();
  } finally {
    if (saved) {
      Object.defineProperty(globalThis, 'sessionStorage', saved);
    } else {
      delete globalThis.sessionStorage;
    }
  }
}

test('builds the URL from the topics and the stored cursor', () => {
  FakeEventSource.created = [];
  const storage = memoryStorage({ cursor: '41' });
  subscribe('/events', { topics: ['orders', 'a b'], storage, key: 'cursor', EventSource: FakeEventSource });
  assert.equal(last().url, '/events?topic=orders&topic=a%20b&last-id=41');

  subscribe('/events', { storage: memoryStorage({ k: '4&x' }), key: 'k', EventSource: FakeEventSource });
  assert.equal(last().url, '/events?last-id=4%26x', 'a stored cursor is encoded');
  assert.deepEqual(last().init, { withCredentials: false });

  subscribe('/events?topic=x', { storage: null, EventSource: FakeEventSource });
  assert.equal(last().url, '/events?topic=x', 'no cursor, no topics: the URL is kept');

  subscribe('/events?topic=x', {
    topics: ['y'],
    storage: memoryStorage({ 'nuts:/events?topic=x&topic=y': '7' }),
    withCredentials: true,
    EventSource: FakeEventSource,
  });
  assert.equal(last().url, '/events?topic=x&topic=y&last-id=7', 'the default key is the URL with its topics');
  assert.deepEqual(last().init, { withCredentials: true });
});

test('keeps the cursor of every event that carries one, reset included', () => {
  FakeEventSource.created = [];
  const storage = memoryStorage();
  const seen = [];
  const states = [];
  const stream = subscribe('/events', {
    topics: ['orders'],
    storage,
    key: 'k',
    EventSource: FakeEventSource,
    onState: (state) => states.push(state),
    on: {
      connected: (data, event) => seen.push(['connected', data, event.lastEventId]),
      message: (data) => seen.push(['message', data]),
      orders: (data) => seen.push(['orders', data]),
      reset: (data) => seen.push(['reset', data]),
    },
  });
  const es = last();
  es.emit('connected', '{"topics":["orders"]}', '41');
  assert.equal(storage.getItem('k'), '41', 'the handshake carries the start position');
  es.emit('message', '{"topic":"orders","payload":{"n":1}}', '42');
  es.emit('orders', '{"topic":"orders"}', '43');
  assert.equal(storage.getItem('k'), '43');
  es.emit('reset', '{"reason":"stream_recreated"}', '0');
  assert.equal(storage.getItem('k'), '0', 'the reset makes a reload replay the new stream');
  assert.equal(stream.lastEventId, '0');
  es.emit('message', 'not json', '');
  assert.equal(storage.getItem('k'), '0', 'an event without an ID keeps the cursor');
  assert.deepEqual(seen, [
    ['connected', { topics: ['orders'] }, '41'],
    ['message', { topic: 'orders', payload: { n: 1 } }],
    ['orders', { topic: 'orders' }],
    ['reset', { reason: 'stream_recreated' }],
    ['message', 'not json'],
  ]);
  assert.deepEqual(states, ['connecting', 'open']);
});

test('follows NUTS events and messages without listeners for them', () => {
  FakeEventSource.created = [];
  const stream = subscribe('/events', { storage: null, EventSource: FakeEventSource, on: { orders() {} } });
  last().emit('message', '{}', '5');
  assert.equal(stream.lastEventId, '5');
  last().emit('reset', '{}', '0');
  assert.equal(stream.lastEventId, '0');
  last().emit('connected', '{}', '3');
  assert.equal(stream.lastEventId, '3');
  last().emit('invoices', '{}', '9');
  assert.equal(stream.lastEventId, '3', 'an event name without a listener is not seen');
});

test('raw payloads are passed as they are, NUTS events still parsed', () => {
  FakeEventSource.created = [];
  const seen = [];
  const on = { connected: (data) => seen.push(data), reset: (data) => seen.push(data), message: (data) => seen.push(data) };
  subscribe('/raw', { raw: true, storage: null, EventSource: FakeEventSource, on });
  last().emit('connected', '{"topics":["a"]}', '1');
  last().emit('message', '{"n":1}', '2');
  last().emit('reset', '{"reason":"stream_rewound"}', '0');
  assert.deepEqual(seen, [{ topics: ['a'] }, '{"n":1}', { reason: 'stream_rewound' }]);
});

test('restarts a closed EventSource from the latest cursor, and close stops it', () => {
  mock.timers.enable({ apis: ['setTimeout'] });
  try {
    FakeEventSource.created = [];
    const states = [];
    const stream = subscribe('/events', {
      topics: ['orders'],
      storage: null,
      EventSource: FakeEventSource,
      onState: (state) => states.push(state),
    });
    const first = last();
    first.emit('message', '{}', '7');
    first.fail(false);
    assert.equal(FakeEventSource.created.length, 1, 'EventSource retries by itself');
    first.fail(true);
    mock.timers.tick(9999);
    assert.equal(FakeEventSource.created.length, 1, 'the default delay is 10 seconds');
    mock.timers.tick(1);
    assert.equal(FakeEventSource.created.length, 2);
    assert.equal(last().url, '/events?topic=orders&last-id=7');
    first.fail(true); // a late error of the old one changes nothing
    last().fail(true);
    stream.close();
    stream.close();
    mock.timers.tick(20000);
    assert.equal(FakeEventSource.created.length, 2, 'a closed subscription does not restart');
    assert.ok(last().closed);
    last().fail(true);
    assert.deepEqual(states, ['connecting', 'retrying', 'restarting', 'connecting', 'restarting', 'closed']);
  } finally {
    mock.timers.reset();
  }
});

test('restartDelay sets the wait before a new EventSource', () => {
  mock.timers.enable({ apis: ['setTimeout'] });
  try {
    FakeEventSource.created = [];
    subscribe('/events', { restartDelay: 1000, storage: null, EventSource: FakeEventSource });
    last().fail(true);
    mock.timers.tick(999);
    assert.equal(FakeEventSource.created.length, 1);
    mock.timers.tick(1);
    assert.equal(FakeEventSource.created.length, 2);
  } finally {
    mock.timers.reset();
  }
});

test('the default storage is the session storage', () => {
  FakeEventSource.created = [];
  const storage = memoryStorage({ 'nuts:/events?topic=a': '12' });
  withSessionStorage({ value: storage }, () => {
    subscribe('/events', { topics: ['a'], EventSource: FakeEventSource });
  });
  assert.equal(last().url, '/events?topic=a&last-id=12');
  last().emit('message', '{}', '13');
  assert.equal(storage.getItem('nuts:/events?topic=a'), '13');
});

test('a storage that throws leaves the cursor in memory', () => {
  FakeEventSource.created = [];
  const broken = {
    getItem() { throw new Error('denied'); },
    setItem() { throw new Error('full'); },
  };
  const stream = subscribe('/events', { storage: broken, EventSource: FakeEventSource });
  assert.equal(last().url, '/events');
  last().emit('message', '{}', '9');
  assert.equal(stream.lastEventId, '9');

  withSessionStorage({ get() { throw new Error('SecurityError'); } }, () => {
    subscribe('/events', { EventSource: FakeEventSource });
  });
  last().emit('message', '{}', '10');
  assert.equal(last().url, '/events', 'without a session storage the cursor lives in memory');
});
