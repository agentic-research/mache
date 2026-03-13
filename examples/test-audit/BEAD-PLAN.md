# Bead Plan

### fix-sql-injection (T2)
**Dimension:** security
Parameterize all SQL queries in auth handler. Replace string concatenation with prepared statements.

### add-payment-tests (T3)
**Dimension:** testing
Write comprehensive test suite for payment processing module. Cover happy path, edge cases, and error scenarios.

### add-rate-limiting (T1)
**Dimension:** security
Add express-rate-limit middleware to all public API endpoints. Configure per-endpoint limits.

### standardize-errors (T2)
**Dimension:** architecture
Create unified error response class. Migrate all endpoints to consistent format.
