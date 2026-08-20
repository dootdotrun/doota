// Live chat updates and the composer.
//
// Deliberately not htmx's SSE extension and deliberately not a renderer. The server
// owns rendering: this listens for "something happened", then fetches the canonical
// HTML from /chat/tail and appends it. So there is exactly one implementation of a
// message bubble, and the live view can never drift from what a page reload shows.
//
// The one exception is streamed text, which is painted directly into a placeholder
// because its whole purpose is immediacy. It is plain text while it streams and is
// replaced by the server's markdown rendering as soon as the message row exists.
(function () {
  var transcript = document.getElementById("transcript");
  var composer = document.getElementById("composer-input");

  // ---------------------------------------------------------------- composer
  //
  // Enter always inserts a newline. Sending is the button, or Ctrl/Cmd+Enter.
  //
  // This is the fix for the thing that made the box unusable on a phone: a mobile
  // keyboard's return key arrives as Enter, so binding Enter to submit left no way
  // to type a second line — every attempt at a line break sent the message
  // half-written.
  //
  // The obvious fix is to branch on whether this is a touch device, and the first
  // version did, using `(pointer: coarse)`. That was rejected: the whole behaviour
  // then rests on a media query being right, and when it is wrong the failure is
  // exactly the bug being fixed, silently, on the one device that matters. This app
  // is built for a phone, so the phone gets the unconditional guarantee and the
  // desktop gets the modifier — Ctrl/Cmd+Enter is the established convention for
  // send-in-a-multiline-box anyway, and the send button never stops working.
  if (composer) {
    // Kept in sync with .composer textarea max-height in app.css.
    var maxHeight = function () {
      return Math.round(window.innerHeight * 0.4);
    };

    var grow = function () {
      composer.style.height = "auto";
      var next = Math.min(composer.scrollHeight, maxHeight());
      composer.style.height = next + "px";
      // Only scroll internally once it has stopped growing, so the caret stays
      // visible in a long message instead of the box silently clipping it.
      composer.style.overflowY = composer.scrollHeight > next ? "auto" : "hidden";
    };

    composer.addEventListener("input", grow);

    composer.addEventListener("keydown", function (e) {
      if (e.key !== "Enter") return;
      // Mid-composition in an IME: Enter is committing a candidate, not sending.
      if (e.isComposing || e.keyCode === 229) return;
      // Ctrl/Cmd+Enter sends. Every other Enter falls through to the default,
      // which is a newline.
      if (!e.metaKey && !e.ctrlKey) return;
      e.preventDefault();
      if (composer.value.trim() !== "") composer.form.requestSubmit();
    });

    // A send leaves the tall box behind for the instant before the redirect lands.
    composer.form.addEventListener("submit", function () {
      window.setTimeout(function () {
        composer.value = "";
        grow();
      }, 0);
    });

    // The viewport height changes when the soft keyboard opens, which changes the
    // cap the box grows to.
    window.addEventListener("resize", grow);

    grow();
  }

  // ------------------------------------------------------------------- live
  if (!transcript || !window.EventSource) return;

  var streaming = document.getElementById("streaming");
  var streamingText = document.getElementById("streaming-text");
  var bar = document.getElementById("agent-bar");
  var stateLabel = document.getElementById("agent-state");

  var lastId = parseInt(transcript.dataset.lastMessageId || "0", 10) || 0;
  var fetching = false;
  var refetch = false;

  function atBottom() {
    return window.innerHeight + window.scrollY >= document.body.offsetHeight - 160;
  }

  function scroll(force) {
    if (force || atBottom()) {
      window.scrollTo(0, document.body.scrollHeight);
    }
  }

  function setState(state, detail) {
    if (!bar || !stateLabel) return;
    if (state === "idle" || state === "") {
      bar.classList.add("hidden");
      return;
    }
    bar.classList.remove("hidden");
    // "thinking" is a real state on this model, not a euphemism: it spends most of
    // every call reasoning before it emits a single character, so a UI that only
    // showed streamed text would look stalled for seconds at a time.
    stateLabel.textContent = detail ? state + " · " + detail : state;
  }

  // Fetch canonical HTML for everything newer than our cursor. Coalesced, because
  // several events commonly land at once and each would otherwise cause a request.
  function tail() {
    if (fetching) {
      refetch = true;
      return;
    }
    fetching = true;

    fetch("/chat/tail?after=" + lastId, { credentials: "same-origin" })
      .then(function (r) {
        var header = r.headers.get("X-Last-Message-Id");
        if (header) lastId = parseInt(header, 10) || lastId;
        return r.text();
      })
      .then(function (html) {
        if (html.trim() !== "") {
          var placeholder = transcript.querySelector(".card");
          if (placeholder) placeholder.remove();
          transcript.insertAdjacentHTML("beforeend", html);
          // The streamed placeholder has been superseded by real rows.
          clearStream();
          scroll(false);
        }
      })
      .catch(function () { /* the next event will retry */ })
      .finally(function () {
        fetching = false;
        if (refetch) {
          refetch = false;
          tail();
        }
      });
  }

  function clearStream() {
    if (!streaming || !streamingText) return;
    streamingText.textContent = "";
    streaming.classList.add("hidden");
  }

  // reload is debounced because the events that need one arrive in bursts, and it
  // refuses to run while there is unsent text in the composer.
  //
  // A reload is how server-rendered controls stay honest, but it also wipes the
  // textarea — so an agent crossing a state boundary while you are half-way
  // through a reply would eat the reply. Deferring until the composer is empty
  // costs a stale Pause button for a few seconds and saves the message.
  var reloadTimer = null;
  var reloadPending = false;

  function composerBusy() {
    return composer && (composer.value.trim() !== "" || document.activeElement === composer);
  }

  function reload() {
    if (composerBusy()) {
      reloadPending = true;
      return;
    }
    if (reloadTimer) return;
    reloadTimer = window.setTimeout(function () { window.location.reload(); }, 80);
  }

  if (composer) {
    // Once the composer is clear again, apply whatever was deferred.
    var drain = function () {
      if (reloadPending && !composerBusy()) {
        reloadPending = false;
        reload();
      }
    };
    composer.addEventListener("blur", drain);
    composer.addEventListener("input", drain);
  }

  var source = new EventSource("/events");

  source.addEventListener("message.delta", function (e) {
    if (!streaming || !streamingText) return;
    try {
      var payload = JSON.parse(e.data);
      if (!payload.text) return;
      streaming.classList.remove("hidden");
      streamingText.textContent += payload.text;
      scroll(false);
    } catch (err) { /* ignore a malformed frame */ }
  });

  source.addEventListener("message.created", function () { tail(); });
  source.addEventListener("message.complete", function () { tail(); });
  source.addEventListener("tool.complete", function () { tail(); });
  source.addEventListener("conversation.cleared", function () { window.location.reload(); });

  source.addEventListener("tool.started", function (e) {
    try {
      var payload = JSON.parse(e.data);
      setState("working", payload.tool || "");
    } catch (err) { setState("working", ""); }
  });

  source.addEventListener("agent.state", function (e) {
    try {
      var payload = JSON.parse(e.data);
      setState(payload.state, payload.detail || "");
      if (payload.state === "idle" || payload.state === "error") {
        clearStream();
        tail();
      }
    } catch (err) { /* ignore */ }
  });

  // Controls and the plan panel are server-rendered from durable state. Refresh
  // after a state boundary so Pause/Resume, the approval buttons, and task
  // progress never rely on stale JS. The plan lives on this screen now, so these
  // are the events that used to drive a second EventSource on its own tab.
  source.addEventListener("run.state", reload);
  // plan.updated is the only plan event the agent publishes. The deleted plan.js
  // listened for "plan.created" and "phase.updated", which are not emitted
  // anywhere, so the board it was refreshing never refreshed.
  source.addEventListener("plan.updated", reload);

  // A reconnect replays structural events from Last-Event-ID, but anything that
  // landed while the connection was down is best caught by one reconciling fetch.
  source.addEventListener("open", function () { tail(); });

  scroll(true);
})();
