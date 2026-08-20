// Plan state is canonical server-rendered HTML. A structural plan/phase/run
// event simply reloads the page; this keeps the live view identical to a reload.
(function () {
  if (!window.EventSource) return;
  var source = new EventSource("/events");
  var pending = false;
  function refresh() {
    if (pending) return;
    pending = true;
    window.setTimeout(function () { window.location.reload(); }, 80);
  }
  source.addEventListener("plan.created", refresh);
  source.addEventListener("phase.updated", refresh);
  source.addEventListener("run.state", refresh);
}());
