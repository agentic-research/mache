package fixturedb

// The row specs a [Builder] accumulates. They are the fixture's CONTENT —
// what is true about the code being modelled — and carry no shape: which
// columns each becomes is decided per producer at emit time (emit.go).

type construct struct {
	id      ConstructID
	source  SourceID
	name    string
	parent  ConstructID
	dirKind bool
}

type defSpec struct {
	token     string
	nodeID    ConstructID
	container ConstructID
	kind      CanonicalKind
	subtree   string
}

type refSpec struct {
	token     string
	from      ConstructID
	at        SiteID
	qualifier string
	subtree   string
}

type astSpec struct {
	nodeID  string
	kind    string
	source  SourceID
	span    Span
	token   string
	subtree string
}

type sourceSpec struct {
	id      SourceID
	lang    string
	content string
	path    string
}

type importSpec struct {
	alias, importPath string
	source            SourceID
}
