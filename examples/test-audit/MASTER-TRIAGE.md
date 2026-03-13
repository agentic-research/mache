# Master Triage

### C1: Unsanitized user input in SQL queries
**Status:** OPEN
**Severity:** critical
**Dimension:** security
Raw SQL string concatenation in auth handler allows injection.

### H1: No test coverage for payment processing
**Status:** OPEN
**Severity:** high
**Dimension:** testing
Payment module has 0% test coverage. Critical business logic untested.

### H2: API endpoints missing rate limiting
**Status:** FIXED
**Severity:** high
**Dimension:** security
All public endpoints lack rate limiting, vulnerable to abuse.

### M1: Inconsistent error response format
**Status:** OPEN
**Severity:** medium
**Dimension:** architecture
Some endpoints return {error: string}, others return {message: string, code: int}.

### L1: Console.log statements in production code
**Status:** DEFERRED
**Severity:** low
**Dimension:** dx
12 console.log statements found across 5 files.
