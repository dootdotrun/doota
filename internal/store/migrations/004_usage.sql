-- Input and reasoning token counts, so the operator can see where they are in the
-- context window instead of guessing.
--
-- token_count already existed and holds completion tokens only, which answers "how
-- much did this reply cost" and not "how full is the conversation". The second
-- question is the one that gets asked, because a 1M-token window that feels
-- exhausted is indistinguishable from one that is, without a number on screen.
--
-- reasoning_tokens is split out because on this model it is usually most of the
-- output, and the difference between "the model had nothing to say" and "the model
-- thought until the budget ran out" is otherwise invisible.
ALTER TABLE message ADD COLUMN IF NOT EXISTS prompt_tokens    integer;
ALTER TABLE message ADD COLUMN IF NOT EXISTS reasoning_tokens integer;

COMMENT ON COLUMN message.prompt_tokens IS
    'Input tokens for the call that produced this message: how full the context was.';
COMMENT ON COLUMN message.reasoning_tokens IS
    'The part of token_count spent reasoning rather than answering.';
