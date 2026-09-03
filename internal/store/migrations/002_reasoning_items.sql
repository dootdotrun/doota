-- Reasoning items, for replay across tool turns on the Responses API.
--
-- 001 created message.reasoning as text, anticipating that a reasoning model would
-- hand back prose worth keeping. It never did, on either transport, and the column
-- was never written to once.
--
-- What the Responses API returns instead is a list of reasoning *items*: an id, an
-- opaque encrypted blob, and an optional summary. They are not human-readable and
-- they are not a message — they are state that has to be handed back verbatim on
-- the next request or the model loses the chain of thought it built. So this is
-- jsonb holding an array, not text holding prose.
--
-- The old column is dropped rather than reused. Storing a JSON array in a column
-- named `reasoning text` would read as prose to everything that touched it, and
-- there is nothing to migrate: it is empty in every row by construction.
ALTER TABLE message DROP COLUMN IF EXISTS reasoning;

ALTER TABLE message ADD COLUMN IF NOT EXISTS reasoning_items jsonb;

COMMENT ON COLUMN message.reasoning_items IS
    'Reasoning items from the Responses API, replayed verbatim on later turns. Opaque; never rendered.';
