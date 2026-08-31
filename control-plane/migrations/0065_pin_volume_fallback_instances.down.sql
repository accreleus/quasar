-- Deliberate no-op. 0065 is a DATA migration with no schema half: it rewrote one
-- column value from 'auto' to 'volume' on instances that were relying on the old
-- silent fallback.
--
-- IT CANNOT BE REVERSED, AND GUESSING WOULD BE WORSE THAN DOING NOTHING. After
-- 0065 runs there is no way to tell an instance it pinned from one an operator
-- deliberately set to 'volume' — both read 'volume' and nothing records which
-- hand wrote it. Flipping every 'volume' instance back to 'auto' would silently
-- re-point a deliberately-legacy instance's future homes at the local driver;
-- flipping none is the safe half of that choice, and it is also the harmless
-- one, because 'volume' behaves identically before and after 0065.
--
-- An operator who wants a pinned instance back on the root-driven driver sets a
-- storage root for the host (Admin → Hosts) and changes the setting themselves.
-- The admin Storage page tells a 'volume' instance both of those things.

SELECT 1;
