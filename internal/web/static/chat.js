// Live chat updates and the composer.
//
// The server owns rendering: this listens for "something happened", then fetches
// the canonical HTML and puts it in place. So there is exactly one implementation
// of a message bubble, and the live view can never drift from what a page reload
// shows.
//
// The one exception is streamed text, which is painted directly into a placeholder
// because its whole purpose is immediacy. It is plain text while it streams and is
// replaced by the server's markdown rendering as soon as the message row exists.
//
// # Nothing here reloads the page
//
// It used to. `run.state` and `plan.updated` both called
// window.location.reload(), because the controls those events change are
// server-rendered and a reload is the honest way to refresh them. The cost was
// everything else a reload does: the EventSource was destroyed and reconnected
// (replaying events, which triggered another fetch), the scroll position was
// thrown away, and the composer was wiped. A single run crosses several state
// boundaries, so this happened repeatedly during normal work, and the page
// appeared to refresh itself at random.
//
// There was a guard for the worst of it — reloads were deferred while the composer
// held text or focus — but deferring meant they queued and then all fired the
// moment you tapped away. The controls are a fragment now, so a state change swaps
// about 40 lines of HTML and touches nothing else.
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
  // The UI is mounted under a prefix; it is published on <body> so this file does
  // not have to agree with a Go constant by hand.
  var appPrefix = document.body.getAttribute("data-app-prefix") || "";

  var lastId = parseInt(transcript.dataset.lastMessageId || "0", 10) || 0;
  var fetching = false;
  var refetch = false;
  // Set when the event that scheduled a fetch means the streamed placeholder has
  // been superseded. Consumed by tail() so the placeholder is emptied in the same
  // DOM update that appends the real row — one reflow instead of a flash.
  var clearOnTail = false;

  // ---------- scroll ----------
  //
  // One measurement, used by both the test and the action. They used to disagree:
  // atBottom() measured document.body.offsetHeight while scroll() jumped to
  // document.body.scrollHeight, which are different numbers, so "are we at the
  // bottom" and "where is the bottom" answered differently.
  function docHeight() {
    return Math.max(
      document.body.scrollHeight,
      document.documentElement.scrollHeight
    );
  }

  function atBottom() {
    return window.innerHeight + window.scrollY >= docHeight() - 160;
  }

  function scroll(force) {
    if (force || atBottom()) window.scrollTo(0, docHeight());
  }

  // ---------- state ----------
  //
  // Elements are looked up on each call rather than cached, because the controls
  // fragment is replaced wholesale by refreshControls() and a cached reference
  // would point at a detached node.
  function setState(state, detail) {
    var bar = document.getElementById("agent-bar");
    var stateLabel = document.getElementById("agent-state");
    if (!bar || !stateLabel) return;

    var spinner = bar.querySelector(".spinner");
    var tick = bar.querySelector(".agent-tick");

    // is-quiet dims the bar in place instead of removing it, so the transcript
    // below never moves when a run starts or stops.
    if (state === "idle" || state === "") {
      bar.classList.add("is-quiet");
      if (spinner) spinner.hidden = true;
      stateLabel.textContent = "idle";
      return;
    }
    bar.classList.remove("is-quiet");

    // "done" is the one terminal state the bar shows rather than dims. Dimming it
    // is what made a completed task look like a task that never ran.
    var finished = state === "done";
    var shipped = finished && detail === "shipped";
    bar.classList.toggle("agent-bar-done", shipped);
    bar.classList.toggle("agent-bar-ended", finished && !shipped);
    if (spinner) spinner.hidden = finished;
    // The tick is server-rendered, so a live state arriving on the same document
    // has to take it back down rather than leave it contradicting the label.
    if (tick) tick.hidden = !shipped;

    if (finished) {
      // Matches what a reload renders, instead of "done · shipped".
      stateLabel.textContent = shipped ? "shipped" : "stopped";
      return;
    }
    // "thinking" is a real state on this model, not a euphemism: it spends most of
    // every call reasoning before it emits a single character, so a UI that only
    // showed streamed text would look stalled for seconds at a time.
    stateLabel.textContent = detail ? state + " · " + detail : state;
  }

  // ---------- transcript ----------
  //
  // Fetch canonical HTML for everything newer than our cursor. Coalesced, because
  // several events commonly land at once and each would otherwise cause a request.
  function tail() {
    if (fetching) {
      refetch = true;
      return;
    }
    fetching = true;
    var clearing = clearOnTail;
    clearOnTail = false;

    fetch(appPrefix + "/chat/tail?after=" + lastId, { credentials: "same-origin" })
      .then(function (r) {
        var header = r.headers.get("X-Last-Message-Id");
        if (header) lastId = parseInt(header, 10) || lastId;
        return r.text();
      })
      .then(function (html) {
        var wasAtBottom = atBottom();
        if (html.trim() !== "") {
          transcript.insertAdjacentHTML("beforeend", html);
        }
        // Emptied together with the append, so the placeholder never disappears
        // ahead of the row that replaces it.
        if (clearing) clearStream();
        if (html.trim() !== "") scroll(wasAtBottom);
      })
      .catch(function () {
        // The next event will retry. Restore the flag so the clear is not lost.
        if (clearing) clearOnTail = true;
      })
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
    streaming.classList.add("is-empty");
  }

  // ---------- controls ----------
  //
  // Swap the run-state controls: the agent bar, the awaiting card, the sandbox
  // banner, the task board and its approval buttons. This is what replaced the
  // full page reload.
  var controlsFetching = false;
  var controlsAgain = false;

  function refreshControls() {
    if (controlsFetching) {
      controlsAgain = true;
      return;
    }
    controlsFetching = true;

    fetch(appPrefix + "/chat/controls", { credentials: "same-origin" })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        var current = document.getElementById("controls");
        if (!current || html.trim() === "") return;
        var wasAtBottom = atBottom();
        current.outerHTML = html;
        applyCanSend();
        // Approval buttons appearing above the transcript would otherwise push the
        // conversation out from under the reader.
        if (wasAtBottom) scroll(true);
      })
      .catch(function () { /* the next event will retry */ })
      .finally(function () {
        controlsFetching = false;
        if (controlsAgain) {
          controlsAgain = false;
          refreshControls();
        }
      });
  }

  // The composer is outside the swapped fragment so a draft survives every
  // refresh. Only its enabled state comes from the server.
  function applyCanSend() {
    var controls = document.getElementById("controls");
    if (!controls || !composer) return;
    var canSend = controls.getAttribute("data-can-send") === "1";
    composer.disabled = !canSend;
    var send = document.querySelector("#composer .send");
    if (send) send.disabled = !canSend;
  }

  // ---------- events ----------
  var source = new EventSource(appPrefix + "/events");

  source.addEventListener("message.delta", function (e) {
    if (!streaming || !streamingText) return;
    try {
      var payload = JSON.parse(e.data);
      if (!payload.text) return;
      var wasAtBottom = atBottom();
      streaming.classList.remove("is-empty");
      streamingText.textContent += payload.text;
      scroll(wasAtBottom);
    } catch (err) { /* ignore a malformed frame */ }
  });

  source.addEventListener("message.created", function () { tail(); });

  // The assistant's message row now exists, so the streamed placeholder it stood
  // in for is finished with. This is the only thing that clears it mid-run.
  //
  // tail() used to clear it unconditionally, which meant any tool result landing
  // during a stream hid a placeholder that was still filling — and the next delta
  // showed it again. That show/hide of a growing block, several times a second,
  // was the shake.
  source.addEventListener("message.complete", function () {
    clearOnTail = true;
    tail();
  });

  source.addEventListener("tool.complete", function () { tail(); });

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
      if (payload.state === "idle" || payload.state === "done" || payload.state === "error") {
        clearOnTail = true;
        tail();
      }
    } catch (err) { /* ignore */ }
  });

  // Controls are server-rendered from durable state, so a state boundary refreshes
  // them rather than trusting local JS.
  //
  // plan.updated is the only plan event the agent publishes. A deleted plan.js
  // listened for "plan.created" and "phase.updated", which are not emitted
  // anywhere, so the board it was refreshing never refreshed.
  source.addEventListener("run.state", refreshControls);
  source.addEventListener("plan.updated", refreshControls);

  // The whole transcript is gone; there is nothing to reconcile incrementally.
  // Operator-initiated and rare, so a real reload is right here.
  source.addEventListener("conversation.cleared", function () {
    window.location.reload();
  });

  // A reconnect replays structural events from Last-Event-ID, but anything that
  // landed while the connection was down is best caught by one reconciling fetch.
  source.addEventListener("open", function () {
    tail();
    refreshControls();
  });

  applyCanSend();
  scroll(true);
})();
