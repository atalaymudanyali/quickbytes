-- Creates the databases that live next to the API's own `quickbytes` database in
-- the same Postgres container:
--
--   quickbytes_test     the API's tests, so running tests never touches dev data
--   payprovider         the mock payment provider's own data (no tables shared
--                       with the API: it plays an outside company)
--   payprovider_test    the provider's tests
--
-- Postgres runs this once, when its volume is first created. `make db` (and so
-- `make up` and `make test`) runs it again on every start, for volumes created
-- before a database was added here. Each line creates its database only if it's
-- missing, so running it again changes nothing. (CREATE DATABASE has no
-- IF NOT EXISTS, so the SELECT builds the command only when needed and psql's
-- \gexec runs whatever comes back.)
SELECT 'CREATE DATABASE quickbytes_test OWNER quickbytes'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'quickbytes_test')\gexec

SELECT 'CREATE DATABASE payprovider OWNER quickbytes'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'payprovider')\gexec

SELECT 'CREATE DATABASE payprovider_test OWNER quickbytes'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'payprovider_test')\gexec
