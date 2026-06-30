package yamsql

// ansiCoreRoster is the HAND-AUTHORED pinned-fact source for Ledger A
// (RFC-165 §4.1). Two kinds of fact, nothing else:
//
//   - ID / Name / Core / Subfeatures — the SQL:2023 Core roster, transcribed
//     VERBATIM from PostgreSQL 18 Appendix D (D.1 + D.2, the Core-marked rows).
//     PostgreSQL 18 enumerates 176 Core feature/subfeature rows (it was 177 in
//     PG13–15; F812 "Basic flagging" lost Core status in PG16). These are facts.
//   - Java — the support level of the FROZEN fdb-relational 4.12.11.0 reference,
//     sourced from SQL_CONFORMANCE.md. A fact about a pinned artifact; it changes
//     only on a tracked Java-pin bump and is cross-checked by the A3 cross-engine
//     lane where a feature is covered there.
//   - NA — true for Core features structurally outside an embedded record-layer
//     SQL surface (host-language binding, cursors, modules, table privileges,
//     SQL-invoked routines, user-defined types). Kept in the roster so the 176
//     denominator is not shopped down, but reported separately, never as a gap.
//
// The Go column is DERIVED from `# ansi:` corpus tags (ansiledger.go) and is
// NEVER set here — a status cannot be hand-typed. That is the whole anti-rot point.
//
// Where a Java assessment is non-obvious it carries a Note; reviewers (and the
// growing A3 cross-engine coverage) refine individual Java facts over time.
var ansiCoreRoster = []AnsiFeature{
	// ── E011 Numeric data types ── Java: no DECIMAL/NUMERIC, no SMALLINT/REAL.
	{
		ID: "E011", Name: "Numeric data types", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E011-01", "E011-02", "E011-03", "E011-04", "E011-05", "E011-06"},
	},
	{ID: "E011-01", Name: "INTEGER and SMALLINT data types", Core: true, Java: SupportPartial, Note: "INTEGER yes; SMALLINT absent"},
	{ID: "E011-02", Name: "REAL, DOUBLE PRECISION, and FLOAT data types", Core: true, Java: SupportPartial, Note: "DOUBLE/FLOAT yes; REAL absent"},
	{ID: "E011-03", Name: "DECIMAL and NUMERIC data types", Core: true, Java: SupportNone, Note: "no exact-decimal type"},
	{ID: "E011-04", Name: "Arithmetic operators", Core: true, Java: SupportFull},
	{ID: "E011-05", Name: "Numeric comparison", Core: true, Java: SupportFull},
	{ID: "E011-06", Name: "Implicit casting among the numeric data types", Core: true, Java: SupportFull},

	// ── E021 Character data types ── Java: STRING type; almost no string functions.
	{
		ID: "E021", Name: "Character data types", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E021-01", "E021-02", "E021-03", "E021-04", "E021-05", "E021-06", "E021-07", "E021-08", "E021-09", "E021-10", "E021-11", "E021-12"},
	},
	{ID: "E021-01", Name: "CHARACTER data type", Core: true, Java: SupportNone, Note: "no fixed-length CHAR(n)"},
	{ID: "E021-02", Name: "CHARACTER VARYING data type", Core: true, Java: SupportPartial, Note: "STRING is variable-length; VARCHAR keyword not accepted"},
	{ID: "E021-03", Name: "Character literals", Core: true, Java: SupportFull},
	{ID: "E021-04", Name: "CHARACTER_LENGTH function", Core: true, Java: SupportNone},
	{ID: "E021-05", Name: "OCTET_LENGTH function", Core: true, Java: SupportNone},
	{ID: "E021-06", Name: "SUBSTRING function", Core: true, Java: SupportNone},
	{ID: "E021-07", Name: "Character concatenation", Core: true, Java: SupportNone},
	{ID: "E021-08", Name: "UPPER and LOWER functions", Core: true, Java: SupportNone},
	{ID: "E021-09", Name: "TRIM function", Core: true, Java: SupportNone},
	{ID: "E021-10", Name: "Implicit casting among the character string types", Core: true, Java: SupportPartial},
	{ID: "E021-11", Name: "POSITION function", Core: true, Java: SupportNone},
	{ID: "E021-12", Name: "Character comparison", Core: true, Java: SupportFull},

	// ── E031 Identifiers ── fdb uppercases; delimited-identifier handling limited.
	{
		ID: "E031", Name: "Identifiers", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E031-01", "E031-02", "E031-03"},
	},
	{ID: "E031-01", Name: "Delimited identifiers", Core: true, Java: SupportPartial, Note: "quoted-identifier normalization gaps"},
	{ID: "E031-02", Name: "Lower case identifiers", Core: true, Java: SupportPartial, Note: "identifiers upper-cased"},
	{ID: "E031-03", Name: "Trailing underscore", Core: true, Java: SupportFull},

	// ── E051 Basic query specification ── Java: full except DISTINCT (Java rejects most).
	{
		ID: "E051", Name: "Basic query specification", Core: true, Java: SupportFull,
		Subfeatures: []string{"E051-01", "E051-02", "E051-04", "E051-05", "E051-06", "E051-07", "E051-08", "E051-09"},
	},
	{ID: "E051-01", Name: "SELECT DISTINCT", Core: true, Java: SupportPartial, Note: "Java rejects most SELECT DISTINCT; Go implements it"},
	{ID: "E051-02", Name: "GROUP BY clause", Core: true, Java: SupportFull},
	{ID: "E051-04", Name: "GROUP BY can contain columns not in <select list>", Core: true, Java: SupportFull},
	{ID: "E051-05", Name: "Select list items can be renamed", Core: true, Java: SupportFull},
	{ID: "E051-06", Name: "HAVING clause", Core: true, Java: SupportFull},
	{ID: "E051-07", Name: "Qualified * in select list", Core: true, Java: SupportFull},
	{ID: "E051-08", Name: "Correlation names in the FROM clause", Core: true, Java: SupportFull},
	{ID: "E051-09", Name: "Rename columns in the FROM clause", Core: true, Java: SupportFull},

	// ── E061 Basic predicates and search conditions ──
	{
		ID: "E061", Name: "Basic predicates and search conditions", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E061-01", "E061-02", "E061-03", "E061-04", "E061-05", "E061-06", "E061-07", "E061-08", "E061-09", "E061-11", "E061-12", "E061-13", "E061-14"},
	},
	{ID: "E061-01", Name: "Comparison predicate", Core: true, Java: SupportFull},
	{ID: "E061-02", Name: "BETWEEN predicate", Core: true, Java: SupportFull},
	{ID: "E061-03", Name: "IN predicate with list of values", Core: true, Java: SupportFull},
	{ID: "E061-04", Name: "LIKE predicate", Core: true, Java: SupportFull},
	{ID: "E061-05", Name: "LIKE predicate ESCAPE clause", Core: true, Java: SupportFull},
	{ID: "E061-06", Name: "NULL predicate", Core: true, Java: SupportFull},
	{ID: "E061-07", Name: "Quantified comparison predicate", Core: true, Java: SupportNone, Note: "ANY/ALL not supported"},
	{ID: "E061-08", Name: "EXISTS predicate", Core: true, Java: SupportFull},
	{ID: "E061-09", Name: "Subqueries in comparison predicate", Core: true, Java: SupportNone, Note: "correlated scalar subquery rejected"},
	{ID: "E061-11", Name: "Subqueries in IN predicate", Core: true, Java: SupportNone},
	{ID: "E061-12", Name: "Subqueries in quantified comparison predicate", Core: true, Java: SupportNone},
	{ID: "E061-13", Name: "Correlated subqueries", Core: true, Java: SupportPartial, Note: "correlated EXISTS yes; correlated scalar no"},
	{ID: "E061-14", Name: "Search condition", Core: true, Java: SupportFull},

	// ── E071 Basic query expressions (set operators) ── Java: UNION ALL only.
	{
		ID: "E071", Name: "Basic query expressions", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E071-01", "E071-02", "E071-03", "E071-05", "E071-06"},
	},
	{ID: "E071-01", Name: "UNION DISTINCT table operator", Core: true, Java: SupportNone},
	{ID: "E071-02", Name: "UNION ALL table operator", Core: true, Java: SupportFull},
	{ID: "E071-03", Name: "EXCEPT DISTINCT table operator", Core: true, Java: SupportNone},
	{ID: "E071-05", Name: "Columns combined via table operators need not have exactly the same data type", Core: true, Java: SupportPartial},
	{ID: "E071-06", Name: "Table operators in subqueries", Core: true, Java: SupportPartial},

	// ── E081 Basic Privileges ── NA: no SQL privilege layer in the record-layer surface.
	{
		ID: "E081", Name: "Basic Privileges", Core: true, Java: SupportNone, NA: true,
		Subfeatures: []string{"E081-01", "E081-02", "E081-03", "E081-04", "E081-05", "E081-06", "E081-07", "E081-08", "E081-09", "E081-10"},
	},
	{ID: "E081-01", Name: "SELECT privilege", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-02", Name: "DELETE privilege", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-03", Name: "INSERT privilege at the table level", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-04", Name: "UPDATE privilege at the table level", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-05", Name: "UPDATE privilege at the column level", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-06", Name: "REFERENCES privilege at the table level", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-07", Name: "REFERENCES privilege at the column level", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-08", Name: "WITH GRANT OPTION", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-09", Name: "USAGE privilege", Core: true, Java: SupportNone, NA: true},
	{ID: "E081-10", Name: "EXECUTE privilege", Core: true, Java: SupportNone, NA: true},

	// ── E091 Set functions ── Java: all but the DISTINCT quantifier.
	{
		ID: "E091", Name: "Set functions", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E091-01", "E091-02", "E091-03", "E091-04", "E091-05", "E091-06", "E091-07"},
	},
	{ID: "E091-01", Name: "AVG", Core: true, Java: SupportFull},
	{ID: "E091-02", Name: "COUNT", Core: true, Java: SupportFull},
	{ID: "E091-03", Name: "MAX", Core: true, Java: SupportFull},
	{ID: "E091-04", Name: "MIN", Core: true, Java: SupportFull},
	{ID: "E091-05", Name: "SUM", Core: true, Java: SupportFull},
	{ID: "E091-06", Name: "ALL quantifier", Core: true, Java: SupportFull},
	{ID: "E091-07", Name: "DISTINCT quantifier", Core: true, Java: SupportNone, Note: "COUNT(DISTINCT) rejected (0A000) in both engines"},

	// ── E101 Basic data manipulation ──
	{
		ID: "E101", Name: "Basic data manipulation", Core: true, Java: SupportFull,
		Subfeatures: []string{"E101-01", "E101-03", "E101-04"},
	},
	{ID: "E101-01", Name: "INSERT statement", Core: true, Java: SupportFull},
	{ID: "E101-03", Name: "Searched UPDATE statement", Core: true, Java: SupportFull},
	{ID: "E101-04", Name: "Searched DELETE statement", Core: true, Java: SupportFull},

	// ── E111 Single row SELECT statement ──
	{ID: "E111", Name: "Single row SELECT statement", Core: true, Java: SupportFull},

	// ── E121 Basic cursor support ── NA: JDBC/driver cursors, not the SQL surface.
	{
		ID: "E121", Name: "Basic cursor support", Core: true, Java: SupportNone, NA: true,
		Subfeatures: []string{"E121-01", "E121-02", "E121-03", "E121-04", "E121-06", "E121-07", "E121-08", "E121-10", "E121-17"},
	},
	{ID: "E121-01", Name: "DECLARE CURSOR", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-02", Name: "ORDER BY columns need not be in select list", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-03", Name: "Value expressions in ORDER BY clause", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-04", Name: "OPEN statement", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-06", Name: "Positioned UPDATE statement", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-07", Name: "Positioned DELETE statement", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-08", Name: "CLOSE statement", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-10", Name: "FETCH statement implicit NEXT", Core: true, Java: SupportNone, NA: true},
	{ID: "E121-17", Name: "WITH HOLD cursors", Core: true, Java: SupportNone, NA: true},

	// ── E131 Null value support ──
	{ID: "E131", Name: "Null value support (nulls in lieu of values)", Core: true, Java: SupportFull},

	// ── E141 Basic integrity constraints ── Java: NOT NULL/UNIQUE/PK; no FK/CHECK/default.
	{
		ID: "E141", Name: "Basic integrity constraints", Core: true, Java: SupportPartial,
		Subfeatures: []string{"E141-01", "E141-02", "E141-03", "E141-04", "E141-06", "E141-07", "E141-08", "E141-10"},
	},
	{ID: "E141-01", Name: "NOT NULL constraints", Core: true, Java: SupportFull},
	{ID: "E141-02", Name: "UNIQUE constraints of NOT NULL columns", Core: true, Java: SupportFull},
	{ID: "E141-03", Name: "PRIMARY KEY constraints", Core: true, Java: SupportFull},
	{ID: "E141-04", Name: "Basic FOREIGN KEY constraint (NO ACTION default)", Core: true, Java: SupportNone},
	{ID: "E141-06", Name: "CHECK constraints", Core: true, Java: SupportNone},
	{ID: "E141-07", Name: "Column defaults", Core: true, Java: SupportNone},
	{ID: "E141-08", Name: "NOT NULL inferred on PRIMARY KEY", Core: true, Java: SupportFull},
	{ID: "E141-10", Name: "Names in a foreign key can be specified in any order", Core: true, Java: SupportNone},

	// ── E151/E152 Transaction support ── NA: managed at the connection/FDB layer.
	{
		ID: "E151", Name: "Transaction support", Core: true, Java: SupportNone, NA: true,
		Subfeatures: []string{"E151-01", "E151-02"}, Note: "transactions are per-connection/FDB, not SQL statements in this surface",
	},
	{ID: "E151-01", Name: "COMMIT statement", Core: true, Java: SupportNone, NA: true},
	{ID: "E151-02", Name: "ROLLBACK statement", Core: true, Java: SupportNone, NA: true},
	{
		ID: "E152", Name: "Basic SET TRANSACTION statement", Core: true, Java: SupportNone, NA: true,
		Subfeatures: []string{"E152-01", "E152-02"},
	},
	{ID: "E152-01", Name: "SET TRANSACTION: ISOLATION LEVEL SERIALIZABLE", Core: true, Java: SupportNone, NA: true},
	{ID: "E152-02", Name: "SET TRANSACTION: READ ONLY and READ WRITE", Core: true, Java: SupportNone, NA: true},

	// ── E153 Updatable queries with subqueries ──
	{ID: "E153", Name: "Updatable queries with subqueries", Core: true, Java: SupportPartial, Note: "DML WHERE EXISTS yes"},

	// ── E161 SQL comments using leading double minus ──
	{ID: "E161", Name: "SQL comments using leading double minus", Core: true, Java: SupportFull},

	// ── E171 SQLSTATE support ── a parity strength (ExceptionUtil 1:1 port).
	{ID: "E171", Name: "SQLSTATE support", Core: true, Java: SupportFull},

	// ── E182 Host language binding ── NA: module/embedded language.
	{ID: "E182", Name: "Host language binding", Core: true, Java: SupportNone, NA: true},

	// ── F021 Basic information schema ── Java none; Go INFORMATION_SCHEMA extension.
	{
		ID: "F021", Name: "Basic information schema", Core: true, Java: SupportNone,
		Subfeatures: []string{"F021-01", "F021-02", "F021-03", "F021-04", "F021-05", "F021-06"}, Note: "Go-only INFORMATION_SCHEMA extension",
	},
	{ID: "F021-01", Name: "COLUMNS view", Core: true, Java: SupportNone},
	{ID: "F021-02", Name: "TABLES view", Core: true, Java: SupportNone},
	{ID: "F021-03", Name: "VIEWS view", Core: true, Java: SupportNone},
	{ID: "F021-04", Name: "TABLE_CONSTRAINTS view", Core: true, Java: SupportNone},
	{ID: "F021-05", Name: "REFERENTIAL_CONSTRAINTS view", Core: true, Java: SupportNone},
	{ID: "F021-06", Name: "CHECK_CONSTRAINTS view", Core: true, Java: SupportNone},

	// ── F031 Basic schema manipulation ── Java: CREATE/DROP via templates; no GRANT.
	{
		ID: "F031", Name: "Basic schema manipulation", Core: true, Java: SupportPartial,
		Subfeatures: []string{"F031-01", "F031-02", "F031-03", "F031-04", "F031-13", "F031-16", "F031-19"},
	},
	{ID: "F031-01", Name: "CREATE TABLE statement to create persistent base tables", Core: true, Java: SupportFull, Note: "inside CREATE SCHEMA TEMPLATE"},
	{ID: "F031-02", Name: "CREATE VIEW statement", Core: true, Java: SupportPartial},
	{ID: "F031-03", Name: "GRANT statement", Core: true, Java: SupportNone},
	{ID: "F031-04", Name: "ALTER TABLE statement: ADD COLUMN clause", Core: true, Java: SupportPartial, Note: "schema evolution"},
	{ID: "F031-13", Name: "DROP TABLE statement: RESTRICT clause", Core: true, Java: SupportFull},
	{ID: "F031-16", Name: "DROP VIEW statement: RESTRICT clause", Core: true, Java: SupportPartial},
	{ID: "F031-19", Name: "REVOKE statement: RESTRICT clause", Core: true, Java: SupportNone},

	// ── F041 Basic joined table ── Java full (LEFT/RIGHT OUTER added in 4.12).
	{
		ID: "F041", Name: "Basic joined table", Core: true, Java: SupportFull,
		Subfeatures: []string{"F041-01", "F041-02", "F041-03", "F041-04", "F041-05", "F041-07", "F041-08"},
	},
	{ID: "F041-01", Name: "Inner join (but not necessarily the INNER keyword)", Core: true, Java: SupportFull},
	{ID: "F041-02", Name: "INNER keyword", Core: true, Java: SupportFull},
	{ID: "F041-03", Name: "LEFT OUTER JOIN", Core: true, Java: SupportFull},
	{ID: "F041-04", Name: "RIGHT OUTER JOIN", Core: true, Java: SupportFull},
	{ID: "F041-05", Name: "Outer joins can be nested", Core: true, Java: SupportFull},
	{ID: "F041-07", Name: "The inner table in a left or right outer join can also be used in an inner join", Core: true, Java: SupportFull},
	{ID: "F041-08", Name: "All comparison operators are supported (rather than just =)", Core: true, Java: SupportFull},

	// ── F051 Basic date and time ── Java none; Go DATE/TIMESTAMP extension.
	{
		ID: "F051", Name: "Basic date and time", Core: true, Java: SupportNone,
		Subfeatures: []string{"F051-01", "F051-02", "F051-03", "F051-04", "F051-05", "F051-06", "F051-07", "F051-08"}, Note: "Go-only DATE/TIMESTAMP extension",
	},
	{ID: "F051-01", Name: "DATE data type (including DATE literal)", Core: true, Java: SupportNone},
	{ID: "F051-02", Name: "TIME data type", Core: true, Java: SupportNone},
	{ID: "F051-03", Name: "TIMESTAMP data type", Core: true, Java: SupportNone},
	{ID: "F051-04", Name: "Comparison predicate on DATE, TIME, and TIMESTAMP", Core: true, Java: SupportNone},
	{ID: "F051-05", Name: "Explicit CAST between datetime and character string types", Core: true, Java: SupportNone},
	{ID: "F051-06", Name: "CURRENT_DATE", Core: true, Java: SupportNone},
	{ID: "F051-07", Name: "LOCALTIME", Core: true, Java: SupportNone},
	{ID: "F051-08", Name: "LOCALTIMESTAMP", Core: true, Java: SupportNone},

	// ── F081 UNION and EXCEPT in views ──
	{ID: "F081", Name: "UNION and EXCEPT in views", Core: true, Java: SupportNone},

	// ── F131 Grouped operations ──
	{
		ID: "F131", Name: "Grouped operations", Core: true, Java: SupportFull,
		Subfeatures: []string{"F131-01", "F131-02", "F131-03", "F131-04", "F131-05"},
	},
	{ID: "F131-01", Name: "WHERE, GROUP BY, and HAVING supported with grouped views", Core: true, Java: SupportFull},
	{ID: "F131-02", Name: "Multiple tables supported in queries with grouped views", Core: true, Java: SupportFull},
	{ID: "F131-03", Name: "Set functions supported in queries with grouped views", Core: true, Java: SupportFull},
	{ID: "F131-04", Name: "Subqueries with GROUP BY and HAVING and grouped views", Core: true, Java: SupportFull},
	{ID: "F131-05", Name: "Single row SELECT with GROUP BY and HAVING and grouped views", Core: true, Java: SupportFull},

	// ── F181 Multiple module support ── NA.
	{ID: "F181", Name: "Multiple module support", Core: true, Java: SupportNone, NA: true},

	// ── F201 CAST function ──
	{ID: "F201", Name: "CAST function", Core: true, Java: SupportFull},

	// ── F221 Explicit defaults ──
	{ID: "F221", Name: "Explicit defaults", Core: true, Java: SupportNone, Note: "no DEFAULT clause"},

	// ── F261 CASE expression ── Java: searched CASE; simple CASE mis-evaluates (Go fixes).
	{
		ID: "F261", Name: "CASE expression", Core: true, Java: SupportPartial,
		Subfeatures: []string{"F261-01", "F261-02", "F261-03", "F261-04"},
	},
	{ID: "F261-01", Name: "Simple CASE", Core: true, Java: SupportNone, Note: "Java visitCaseExpressionFunctionCall is a no-op; Go evaluates correctly"},
	{ID: "F261-02", Name: "Searched CASE", Core: true, Java: SupportFull},
	{ID: "F261-03", Name: "NULLIF", Core: true, Java: SupportNone, Note: "rejected 42883 — no function-registry entry (both engines)"},
	{ID: "F261-04", Name: "COALESCE", Core: true, Java: SupportFull},

	// ── F311 Schema definition statement ── Java: fdb CREATE SCHEMA (TEMPLATE) variant.
	{
		ID: "F311", Name: "Schema definition statement", Core: true, Java: SupportPartial,
		Subfeatures: []string{"F311-01", "F311-02", "F311-03", "F311-04", "F311-05"},
	},
	{ID: "F311-01", Name: "CREATE SCHEMA", Core: true, Java: SupportFull, Note: "via CREATE SCHEMA TEMPLATE + CREATE SCHEMA"},
	{ID: "F311-02", Name: "CREATE TABLE for persistent base tables", Core: true, Java: SupportFull},
	{ID: "F311-03", Name: "CREATE VIEW", Core: true, Java: SupportPartial},
	{ID: "F311-04", Name: "CREATE VIEW: WITH CHECK OPTION", Core: true, Java: SupportNone},
	{ID: "F311-05", Name: "GRANT statement", Core: true, Java: SupportNone},

	// ── F471 Scalar subquery values ── Java none; Go extension.
	{ID: "F471", Name: "Scalar subquery values", Core: true, Java: SupportNone, Note: "Go-only subqueryExpressionAtom extension"},

	// ── F481 Expanded NULL predicate ──
	{ID: "F481", Name: "Expanded NULL predicate", Core: true, Java: SupportPartial},

	// ── F501 Features and conformance views ──
	{
		ID: "F501", Name: "Features and conformance views", Core: true, Java: SupportNone,
		Subfeatures: []string{"F501-01", "F501-02"},
	},
	{ID: "F501-01", Name: "SQL_FEATURES view", Core: true, Java: SupportNone},
	{ID: "F501-02", Name: "SQL_SIZING view", Core: true, Java: SupportNone},

	// ── S011 Distinct data types ── NA: user-defined types out of scope.
	{
		ID: "S011", Name: "Distinct data types", Core: true, Java: SupportNone, NA: true,
		Subfeatures: []string{"S011-01"},
	},
	{ID: "S011-01", Name: "USER_DEFINED_TYPES view", Core: true, Java: SupportNone, NA: true},

	// ── T321 Basic SQL-invoked routines ── NA: UDFs/stored procedures out of scope.
	{
		ID: "T321", Name: "Basic SQL-invoked routines", Core: true, Java: SupportNone, NA: true,
		Subfeatures: []string{"T321-01", "T321-02", "T321-03", "T321-04", "T321-05", "T321-06", "T321-07"},
	},
	{ID: "T321-01", Name: "User-defined functions with no overloading", Core: true, Java: SupportNone, NA: true},
	{ID: "T321-02", Name: "User-defined stored procedures with no overloading", Core: true, Java: SupportNone, NA: true},
	{ID: "T321-03", Name: "Function invocation", Core: true, Java: SupportNone, NA: true},
	{ID: "T321-04", Name: "CALL statement", Core: true, Java: SupportNone, NA: true},
	{ID: "T321-05", Name: "RETURN statement", Core: true, Java: SupportNone, NA: true},
	{ID: "T321-06", Name: "ROUTINES view", Core: true, Java: SupportNone, NA: true},
	{ID: "T321-07", Name: "PARAMETERS view", Core: true, Java: SupportNone, NA: true},

	// ── T631 IN predicate with one list element ──
	{ID: "T631", Name: "IN predicate with one list element", Core: true, Java: SupportFull},
}
