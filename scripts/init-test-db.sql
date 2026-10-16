-- Runs once, when the Postgres volume is first created: a separate database for
-- tests, so running tests never touches development data.
CREATE DATABASE quickbytes_test OWNER quickbytes;
