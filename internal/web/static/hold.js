// Hold-to-confirm for destructive buttons.
//
// A button with data-hold="<ms>" must be pressed and held for that long before it
// submits. On a phone this is better than a confirm dialog: no modal to mis-tap,
// and the delay is the confirmation. A plain click does nothing, so a stray tap
// while scrolling cannot delete anything.
//
// Progress is shown by filling the button from the left via a CSS custom
// property, so there is feedback without a second element.
(function () {
  'use strict';

  var active = null;

  function reset(btn) {
    if (!btn) return;
    btn.style.removeProperty('--hold-progress');
    btn.classList.remove('holding');
    if (btn.dataset.holdIdleLabel) {
      btn.textContent = btn.dataset.holdIdleLabel;
    }
  }

  function stop() {
    if (!active) return;
    cancelAnimationFrame(active.frame);
    reset(active.btn);
    active = null;
  }

  function start(btn, event) {
    // Ignore anything but a primary press.
    if (event.button !== undefined && event.button !== 0) return;

    var duration = parseInt(btn.dataset.hold, 10);
    if (!duration || duration < 0) return;

    event.preventDefault();
    stop();

    if (!btn.dataset.holdIdleLabel) {
      btn.dataset.holdIdleLabel = btn.textContent.trim();
    }
    if (btn.dataset.holdActive) {
      btn.textContent = btn.dataset.holdActive;
    }
    btn.classList.add('holding');

    var startedAt = performance.now();
    active = { btn: btn, frame: 0 };

    function tick(now) {
      if (!active || active.btn !== btn) return;

      var progress = Math.min((now - startedAt) / duration, 1);
      btn.style.setProperty('--hold-progress', (progress * 100).toFixed(1) + '%');

      if (progress >= 1) {
        var form = btn.form;
        active = null;
        reset(btn);
        if (form) {
          // requestSubmit runs validation and fires submit handlers, unlike
          // form.submit().
          if (form.requestSubmit) {
            form.requestSubmit(btn);
          } else {
            form.submit();
          }
        }
        return;
      }
      active.frame = requestAnimationFrame(tick);
    }

    active.frame = requestAnimationFrame(tick);
  }

  document.addEventListener('pointerdown', function (e) {
    var btn = e.target.closest('[data-hold]');
    if (btn) start(btn, e);
  });

  // Any of these means the finger left before the hold completed.
  ['pointerup', 'pointercancel', 'pointerleave'].forEach(function (type) {
    document.addEventListener(type, stop);
  });

  // A click that did not come from a completed hold must not submit.
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-hold]');
    if (btn) e.preventDefault();
  });

  // Keyboard users get a normal confirm: holding a key is not a real gesture.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    var btn = e.target.closest('[data-hold]');
    if (!btn || !btn.form) return;
    e.preventDefault();
    var label = btn.dataset.holdIdleLabel || btn.textContent.trim();
    if (window.confirm(label + '?')) {
      btn.form.requestSubmit ? btn.form.requestSubmit(btn) : btn.form.submit();
    }
  });
})();
