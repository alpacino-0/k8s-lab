-- The one-time claim that this install has an owner.
--
-- Derivable, in principle: "bootstrapped" is "some membership has role owner".
-- It is a table anyway, because the derived form is only atomic under an
-- isolation level neither engine gives by default — two concurrent bootstraps
-- would each read no owner, each insert one, and each be told it succeeded.
-- A singleton row makes "exactly once" a constraint the engine enforces rather
-- than an assumption about how the caller configured its transactions.
--
-- The CHECK is what makes it a singleton rather than a table that happens to
-- have one row in it.
CREATE TABLE bootstrap (
  id          TEXT PRIMARY KEY CHECK (id = 'singleton'),
  claimed_at  TEXT NOT NULL
);
