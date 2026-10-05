-- Privileges for the application role. Run as the schema owner after
-- db/schema.sql, with the role name passed as a psql variable:
--
--   psql -v ON_ERROR_STOP=1 -v app_role=ledger_app -f db/grants.sql
--
-- The application role can read everything and append to the ledger, but it
-- owns nothing: it cannot alter tables, disable triggers, or UPDATE/DELETE
-- posted journal entries, lines or import records.

BEGIN;

REVOKE ALL ON ALL TABLES IN SCHEMA public FROM :"app_role";
GRANT USAGE ON SCHEMA public TO :"app_role";

GRANT SELECT                          ON currencies                              TO :"app_role";
GRANT SELECT, INSERT, UPDATE          ON accounts                                TO :"app_role";
GRANT SELECT, INSERT                  ON journal_entries, transactions, csv_imports TO :"app_role";
GRANT SELECT, INSERT, UPDATE          ON users                                   TO :"app_role";
GRANT SELECT, INSERT, UPDATE, DELETE  ON sessions                                TO :"app_role";

-- Identity columns draw from sequences (SELECT lets pg_dump read them for backups).
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO :"app_role";

COMMIT;
