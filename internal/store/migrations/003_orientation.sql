-- Durable facts about the repository, learned once instead of every conversation.
--
-- Separate from project.memories on purpose. Memories are the operator's
-- conventions and preferences — things they said. This is what the agent worked out
-- for itself by reading the project: how to build it, how to run its tests, where
-- things live, what the local idioms are.
--
-- Keeping them apart matters because they have different lifetimes and different
-- authorities. An operator preference is true until the operator changes their mind;
-- a fact about the build is true until someone edits the build. Merging them into
-- one blob would mean the agent rewriting the operator's stated preferences every
-- time it re-read a Makefile.
--
-- Text rather than jsonb: unlike reasoning items this is meant to be read, and
-- edited, by a person. It goes into the system prompt verbatim.
ALTER TABLE project ADD COLUMN IF NOT EXISTS orientation text NOT NULL DEFAULT '';

COMMENT ON COLUMN project.orientation IS
    'What the agent learned about this repository: build and test commands, layout, conventions. Operator-editable.';
