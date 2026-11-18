-- How a checkout becomes something that can run.
--
-- Services declared a command to start and nothing declared how to produce the
-- thing that command runs. The first app written against the contract had to
-- pretend its start command was also its build, which works for an interpreted
-- project and for nothing else.
ALTER TABLE projects ADD COLUMN build_command TEXT NOT NULL DEFAULT '';
