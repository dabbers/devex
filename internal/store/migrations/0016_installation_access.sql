-- Who an installation lets in, as JSON: {"mode": "public"|"internal"|"hybrid",
-- "public": ["/path/", ...]}. The edge enforces it before the app sees a
-- request.
--
-- Empty for installations made before there was a policy. Those were public
-- -- the edge refused nobody -- and they stay public until their owner
-- chooses, rather than having their public pages start demanding a sign-in
-- on the day this ships.
ALTER TABLE installations ADD COLUMN access TEXT NOT NULL DEFAULT '';
