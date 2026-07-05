/* NUTS — premium-dark motion layer. Vanilla, deferred, ~4KB.
   Every effect self-gates on prefers-reduced-motion and degrades to a
   legible static state; all visual classes it touches are hand-written
   in assets/css/main.css (purge-immune). */
(function () {
  "use strict";
  var RM = matchMedia("(prefers-reduced-motion:reduce)");
  var hasJS = document.documentElement.classList.contains("js");

  function esc(s) {
    return String(s).replace(/[&<>]/g, function (c) {
      return c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;";
    });
  }

  /* tiny JSON colorizer — operates only on app-generated objects */
  function colorize(v) {
    if (typeof v === "number") return '<span class="sse__num">' + v + "</span>";
    if (typeof v === "string") return '<span class="sse__str">"' + esc(v) + '"</span>';
    if (Array.isArray(v)) return "[" + v.map(colorize).join(",") + "]";
    return (
      "{" +
      Object.keys(v)
        .map(function (k) {
          return '"' + esc(k) + '":' + colorize(v[k]);
        })
        .join(",") +
      "}"
    );
  }

  /* ---------- Effect 1: live SSE demo ---------- */
  function initSSE() {
    var el = document.getElementById("sse-demo"),
      body = document.getElementById("sse-body");
    if (!el || !body) return;
    var topics = ["events.orders", "events.telemetry", "events.payments", "events.audit", "events.signups"];
    var seq = 1041,
      timer = null;
    var mk = function (t) {
      return {
        orders: { total: +(9 + Math.random() * 90).toFixed(2), status: ["created", "paid", "shipped"][(Math.random() * 3) | 0] },
        telemetry: { cpu: (Math.random() * 100) | 0, host: "edge-" + (1 + ((Math.random() * 4) | 0)) },
        payments: { status: "captured", amt: (Math.random() * 200) | 0 },
        audit: { actor: "svc-" + ((Math.random() * 9) | 0), act: "rotate" },
        signups: { plan: ["free", "pro", "team"][(Math.random() * 3) | 0] }
      }[t.split(".")[1]];
    };
    function row() {
      var t = topics[(Math.random() * topics.length) | 0];
      seq++;
      var r = document.createElement("div");
      r.className = "sse__row sse__row--in";
      r.innerHTML =
        '<span class="sse__seq">id:' + seq + "</span> " +
        '<span class="sse__evt">event:message</span> ' +
        '<span class="sse__topic">' + t + "</span> " +
        '<span class="sse__json">' + colorize({ topic: t, payload: mk(t) }) + "</span>";
      body.appendChild(r);
      while (body.children.length > 6) body.removeChild(body.firstChild);
      r.addEventListener("animationend", function () { r.classList.remove("sse__row--in"); }, { once: true });
    }
    if (RM.matches) { body.innerHTML = ""; for (var i = 0; i < 6; i++) row(); return; } // static console
    body.innerHTML = "";
    for (var s = 0; s < 4; s++) row(); // seed so it is never empty
    function play() {
      if (timer) return;
      (function loop() { row(); timer = setTimeout(loop, 900 + Math.random() * 1200); })();
    }
    function stop() { clearTimeout(timer); timer = null; }
    new IntersectionObserver(function (es) {
      es.forEach(function (e) { e.isIntersecting ? play() : stop(); });
    }, { threshold: 0.15 }).observe(el);
    document.addEventListener("visibilitychange", function () {
      if (document.hidden) { stop(); return; }
      var r = el.getBoundingClientRect();
      if (r.top < innerHeight && r.bottom > 0) play(); // only resume when actually on-screen
    });
  }

  /* ---------- Effect 3: scroll reveal ---------- */
  function initReveal() {
    var nodes = document.querySelectorAll(".reveal");
    if (!hasJS || RM.matches || !("IntersectionObserver" in window)) {
      nodes.forEach(function (n) { n.classList.add("is-in"); });
      return;
    }
    var io = new IntersectionObserver(function (es, o) {
      es.forEach(function (e) {
        if (e.isIntersecting) {
          e.target.classList.add("is-in");
          o.unobserve(e.target);
          e.target.addEventListener("transitionend", function () { e.target.style.willChange = ""; }, { once: true });
        }
      });
    }, { rootMargin: "0px 0px -10% 0px", threshold: 0.12 });
    nodes.forEach(function (n) { n.style.willChange = "opacity,transform"; io.observe(n); });
  }

  /* ---------- Effect 3: parallax (transform-only, rAF-throttled) ---------- */
  function initParallax() {
    if (RM.matches) return;
    var layers = [].slice.call(document.querySelectorAll("[data-parallax]"));
    if (!layers.length) return;
    var ticking = false;
    function frame() {
      var y = scrollY;
      layers.forEach(function (l) {
        var k = parseFloat(l.dataset.speed) || 0.06;
        l.style.transform = "translate3d(0," + Math.min(y * k, 48).toFixed(1) + "px,0)";
      });
      ticking = false;
    }
    addEventListener("scroll", function () { if (!ticking) { requestAnimationFrame(frame); ticking = true; } }, { passive: true });
    frame();
  }

  /* ---------- Micro: card tilt + glow (fine pointers only) ---------- */
  function initTilt() {
    if (RM.matches || !matchMedia("(pointer:fine)").matches) return;
    document.querySelectorAll(".card--tilt").forEach(function (card) {
      var MAX = 5;
      card.addEventListener("pointermove", function (e) {
        var r = card.getBoundingClientRect();
        var px = (e.clientX - r.left) / r.width,
          py = (e.clientY - r.top) / r.height;
        card.style.setProperty("--mx", (px * 100).toFixed(1) + "%");
        card.style.setProperty("--my", (py * 100).toFixed(1) + "%");
        card.style.transition = "none"; // track the pointer 1:1; the CSS ease is for the leave
        card.style.transform =
          "perspective(800px) rotateY(" + ((px - 0.5) * 2 * MAX).toFixed(2) + "deg) rotateX(" + ((0.5 - py) * 2 * MAX).toFixed(2) + "deg)";
      });
      card.addEventListener("pointerleave", function () {
        card.style.transition = ""; // restore CSS transition so it eases back
        card.style.transform = "";
      });
    });
  }

  /* ---------- Header scroll state ---------- */
  function initHeader() {
    var h = document.getElementById("site-header");
    if (!h) return;
    var onScroll = function () { h.classList.toggle("is-scrolled", scrollY > 8); };
    addEventListener("scroll", onScroll, { passive: true });
    onScroll();
  }

  initReveal();
  window.__nutsReveal = true; // reveals are now handled → cancels the head-script safety net
  initParallax();
  initTilt();
  initHeader();
  initSSE();
})();
