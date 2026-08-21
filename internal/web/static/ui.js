// Header actions, on every screen.
//
// Loaded site-wide rather than per page, unlike chat.js: the header is part of
// the shell, so its buttons have to work wherever you are.
(function () {
  "use strict";

  // ---------- reload ----------
  //
  // The PWA occasionally sits on a page whose event stream has quietly died —
  // backgrounded for long enough that the browser dropped the connection without
  // retrying. Reloading the document is the fix, because a fresh document opens a
  // fresh EventSource.
  var refresh = document.getElementById("refresh-action");
  if (refresh) {
    refresh.addEventListener("click", function () {
      // Navigate to the bare path rather than calling reload(). A notice or error
      // banner is carried in the query string, and reloading with it still
      // attached re-announces something that already happened, which reads as the
      // action having run twice.
      var clean = window.location.pathname;
      if (window.location.search || window.location.hash) {
        window.location.replace(clean);
      } else {
        window.location.reload();
      }
    });
  }

  // ---------- clear conversation ----------
  //
  // A modal rather than the hold-to-confirm used by the destructive buttons in
  // Settings. Hold-to-confirm suits a control you have already navigated to on
  // purpose; this one is a single tap away on every screen, and a hard delete of
  // the whole conversation deserves a sentence explaining what survives.
  var modal = document.getElementById("clear-modal");
  var opener = document.getElementById("clear-action");
  if (!modal || !opener) {
    return;
  }

  var lastFocused = null;

  function open() {
    lastFocused = document.activeElement;
    modal.hidden = false;
    document.body.classList.add("modal-open");
    var cancel = modal.querySelector("[data-modal-close]");
    if (cancel) {
      cancel.focus();
    }
  }

  function close() {
    modal.hidden = true;
    document.body.classList.remove("modal-open");
    if (lastFocused && typeof lastFocused.focus === "function") {
      lastFocused.focus();
    }
  }

  opener.addEventListener("click", open);

  modal.addEventListener("click", function (event) {
    // The backdrop is the modal element itself, so a click that did not land on
    // the card is a click outside the dialog.
    if (event.target === modal || event.target.hasAttribute("data-modal-close")) {
      close();
    }
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape" && !modal.hidden) {
      close();
    }
  });
})();
