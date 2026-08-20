// Live chat updates.
//
// Deliberately not htmx's SSE extension and deliberately not a renderer. The server
// owns rendering: this listens for "something happened", then fetches the canonical
// HTML from /chat/tail and appends it. So there is exactly one implementation of a
// message bubble, and the live view can never drift from what a page reload shows.
//
// The one exception is streamed text, which is painted directly into a placeholder
// because its whole purpose is immediacy. It is replaced by the canonical rendering
// as soon as the message row exists.
(function () {
  var transcript = document.getElementById("transcript");
  if (!transcript || !window.EventSource) return;

  var streaming = document.getElementById("streaming");
  var streamingText = document.getElementById("streaming-text");
  var bar = document.getElementById("agent-bar");
  var stateLabel = document.getElementById("agent-state");
  var composer = document.getElementById("composer-input");

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

  source.addEventListener("run.state", function () {
    // Controls are server-rendered from durable state. Refresh after a state
    // boundary so Pause/Resume and the awaiting reason never rely on stale JS.
    window.setTimeout(function () { window.location.reload(); }, 50);
  });

  // A reconnect replays structural events from Last-Event-ID, but anything that
  // landed while the connection was down is best caught by one reconciling fetch.
  source.addEventListener("open", function () { tail(); });

  // Composer: grow with content, and send on Enter while leaving Shift+Enter for a
  // newline. Enter-to-send is what makes this usable one-handed.
  if (composer) {
    var grow = function () {
      composer.style.height = "auto";
      composer.style.height = Math.min(composer.scrollHeight, 160) + "px";
    };
    composer.addEventListener("input", grow);
    composer.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        if (composer.value.trim() !== "") composer.form.requestSubmit();
      }
    });
    grow();
  }

  scroll(true);
})();
