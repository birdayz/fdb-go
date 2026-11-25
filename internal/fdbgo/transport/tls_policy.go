package transport

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// VerifyPolicy is a parsed verify-peers setting, the policy libfdb_c applies to a
// peer certificate (flow/TLSConfig.actor.cpp TLSPolicy). Any rule that matches
// accepts the peer.
type VerifyPolicy struct {
	rules []verifyRule
}

// DefaultVerifyPolicy is libfdb_c's default, Check.Valid=1: the chain must verify
// against the configured roots, and nothing else is checked.
var DefaultVerifyPolicy = &VerifyPolicy{rules: []verifyRule{{verifyCert: true}}}

type matchType int

const (
	matchExact  matchType = iota // =
	matchPrefix                  // >=
	matchSuffix                  // <=
)

type criterion struct {
	oid   asn1.ObjectIdentifier // the DN attribute; nil for subjectAltName
	value string
	match matchType
}

type verifyRule struct {
	verifyCert            bool // Check.Valid; Check.Unexpired is parsed and, as in C++, never read
	subject, issuer, root []criterion
}

// VerifyPeersError reports a verify-peers string libfdb_c refuses to parse.
type VerifyPeersError struct {
	Rule   string
	Reason string
}

func (e *VerifyPeersError) Error() string {
	return fmt.Sprintf("tls: verify peers %q: %s", e.Rule, e.Reason)
}

// PeerVerifyError reports a peer certificate the policy rejects.
type PeerVerifyError struct {
	Reason string
	Err    error // the chain error, when the chain did not verify
}

func (e *PeerVerifyError) Error() string {
	if e.Err != nil {
		return "tls: peer verification failed: " + e.Reason + ": " + e.Err.Error()
	}
	return "tls: peer verification failed: " + e.Reason
}

func (e *PeerVerifyError) Unwrap() error { return e.Err }

// verifyFields are the fields libfdb_c accepts (abbrevToNID, TLSConfig.actor.cpp:478-488).
var verifyFields = map[string]asn1.ObjectIdentifier{
	"C":              {2, 5, 4, 6},
	"CN":             {2, 5, 4, 3},
	"L":              {2, 5, 4, 7},
	"ST":             {2, 5, 4, 8},
	"O":              {2, 5, 4, 10},
	"OU":             {2, 5, 4, 11},
	"UID":            {0, 9, 2342, 19200300, 100, 1, 1},
	"DC":             {0, 9, 2342, 19200300, 100, 1, 25},
	"subjectAltName": nil,
}

// ParseVerifyPeers parses a verify-peers string as set_verify_peers and
// TLSPolicy::Rule do (TLSConfig.actor.cpp:503-611): rules split on '|', each a
// comma-separated list of term[<>]?=value.
func ParseVerifyPeers(s string) (*VerifyPolicy, error) {
	p := &VerifyPolicy{}
	start := 0
	for {
		split := strings.IndexByte(s[start:], '|')
		if split < 0 {
			break
		}
		split += start
		if split > start && s[split-1] == '\\' {
			// libfdb_c loops here forever (TLSConfig.actor.cpp:509-516).
			return nil, &VerifyPeersError{Rule: s, Reason: `escaped "|"`}
		}
		r, err := parseRule(s[start:split])
		if err != nil {
			return nil, err
		}
		p.rules = append(p.rules, r)
		start = split + 1
	}
	r, err := parseRule(s[start:])
	if err != nil {
		return nil, err
	}
	p.rules = append(p.rules, r)
	return p, nil
}

func parseRule(in string) (verifyRule, error) {
	r := verifyRule{verifyCert: true}
	fail := func(reason string) (verifyRule, error) {
		return verifyRule{}, &VerifyPeersError{Rule: in, Reason: reason}
	}
	for s := 0; s < len(in); {
		eq := strings.IndexByte(in[s:], '=')
		if eq <= 0 {
			return fail("expected term=value")
		}
		eq += s
		mt := matchExact
		switch in[eq-1] {
		case '>':
			mt = matchPrefix
		case '<':
			mt = matchSuffix
		}
		term := in[s:eq]
		if mt != matchExact {
			term = in[s : eq-1]
		}

		if strings.HasPrefix(term, "Check.") {
			if eq+2 > len(in) || (eq+2 != len(in) && in[eq+2] != ',') {
				return fail(term + " takes 0 or 1")
			}
			if mt != matchExact {
				return fail("cannot prefix or suffix match " + term)
			}
			var flag bool
			switch in[eq+1] {
			case '0':
			case '1':
				flag = true
			default:
				return fail(term + " takes 0 or 1")
			}
			switch term {
			case "Check.Valid":
				r.verifyCert = flag
			case "Check.Unexpired":
			default:
				return fail("unknown check " + term)
			}
			s = eq + 3
			continue
		}

		scope := &r.subject
		if scopeName, field, ok := strings.Cut(term, "."); ok {
			switch scopeName {
			case "S", "Subject":
			case "I", "Issuer":
				scope = &r.issuer
			case "R", "Root":
				scope = &r.root
			default:
				return fail("unknown scope " + scopeName)
			}
			term = field
		}
		value, end := de4514(in, eq+1)
		if end == eq+1 {
			return fail("bad value for " + term)
		}
		oid, known := verifyFields[term]
		if !known {
			return fail("unknown field " + term)
		}
		c := criterion{oid: oid, value: value, match: mt}
		if !hasCriterion(*scope, c) {
			*scope = append(*scope, c)
		}
		if end != len(in) && in[end] != ',' {
			return fail("expected , after " + term)
		}
		s = end + 1
	}
	return r, nil
}

func hasCriterion(cs []criterion, c criterion) bool {
	for _, x := range cs {
		if x.oid.Equal(c.oid) && x.value == c.value && x.match == c.match {
			return true
		}
	}
	return false
}

// de4514 unescapes the value starting at in[start] as TLSConfig.actor.cpp:384-468
// does, returning it and where it ends. A value ends at an unescaped '"', '+', ',',
// ';', '<', '>', NUL or a bad escape, and loses its trailing unescaped spaces; end
// then points at the first of them, so anything but ',' after it is a parse error.
func de4514(in string, start int) (value string, end int) {
	if start < len(in) && (in[start] == '#' || in[start] == ' ') {
		return "", start
	}
	var out []byte
	spaces := 0
	p := start
loop:
	for p < len(in) {
		switch c := in[p]; c {
		case '\\':
			if p+1 >= len(in) {
				break loop
			}
			if strings.IndexByte(" \"#+,;<=>|\\", in[p+1]) >= 0 {
				out = append(out, in[p+1])
				p += 2
				spaces = 0
				continue
			}
			if p+2 >= len(in) {
				break loop
			}
			hi, lo := hexDigit(in[p+1]), hexDigit(in[p+2])
			if hi < 0 || lo < 0 {
				break loop
			}
			out = append(out, byte(hi<<4|lo))
			p += 3
			spaces = 0
		case '"', '+', ',', ';', '<', '>', 0:
			break loop
		default:
			out = append(out, c)
			if c == ' ' {
				spaces++
			} else {
				spaces = 0
			}
			p++
		}
	}
	return string(out[:len(out)-spaces]), p - spaces
}

func hexDigit(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// Verify applies the policy to the peer's certificates, leaf first, as
// TLSPolicy::verify_peer does: a rule with Check.Valid=0 accepts any peer;
// otherwise the chain must verify against roots and one rule must match it.
// Nil roots trust nothing, as libfdb_c with no CA; never the system roots.
func (p *VerifyPolicy) Verify(certs []*x509.Certificate, roots *x509.CertPool) error {
	for _, r := range p.rules {
		if !r.verifyCert {
			return nil
		}
	}
	if len(certs) == 0 {
		return &PeerVerifyError{Reason: "no peer certificate"}
	}
	if roots == nil {
		roots = x509.NewCertPool()
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	chains, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return &PeerVerifyError{Reason: "chain does not verify", Err: err}
	}
	chains = slices.DeleteFunc(chains, func(chain []*x509.Certificate) bool {
		return slices.ContainsFunc(chain, anyEKUOnly)
	})
	if len(chains) == 0 {
		return &PeerVerifyError{Reason: "chain does not verify", Err: errAnyEKUOnly}
	}
	if len(p.rules) == 0 {
		return nil
	}
	for _, chain := range chains {
		for _, r := range p.rules {
			if r.matches(chain) {
				return nil
			}
		}
	}
	return &PeerVerifyError{Reason: "no rule matches the peer"}
}

// errAnyEKUOnly is the cause when every chain has a certificate whose extended key
// usage is anyExtendedKeyUsage without serverAuth.
var errAnyEKUOnly = errors.New("certificate extended key usage is anyExtendedKeyUsage without serverAuth")

// anyEKUOnly reports a certificate crypto/x509 accepts for serverAuth through
// anyExtendedKeyUsage alone; OpenSSL's ssl_server purpose rejects it (xku_reject).
func anyEKUOnly(c *x509.Certificate) bool {
	if !slices.Contains(c.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return false
	}
	for _, u := range c.ExtKeyUsage {
		switch u {
		case x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageNetscapeServerGatedCrypto, x509.ExtKeyUsageMicrosoftServerGatedCrypto:
			return false
		}
	}
	return true
}

// VerifyConnection returns a tls.Config.VerifyConnection hook applying the policy
// against roots, reading no host name; the config needs InsecureSkipVerify.
func (p *VerifyPolicy) VerifyConnection(roots *x509.CertPool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error { return p.Verify(cs.PeerCertificates, roots) }
}

// matches is check_verify (TLSConfig.actor.cpp:737-790): subject criteria on the
// leaf, issuer criteria on the leaf's issuer name, root criteria on the last
// certificate of the chain. A subjectAltName criterion in any scope reads that
// scope's certificate's own SAN, so I.subjectAltName reads the leaf's.
func (r *verifyRule) matches(chain []*x509.Certificate) bool {
	leaf, root := chain[0], chain[len(chain)-1]
	for _, c := range r.subject {
		if !c.matches(leaf, leaf.Subject.Names) {
			return false
		}
	}
	for _, c := range r.issuer {
		if !c.matches(leaf, leaf.Issuer.Names) {
			return false
		}
	}
	for _, c := range r.root {
		if !c.matches(root, root.Subject.Names) {
			return false
		}
	}
	return true
}

func (c *criterion) matches(cert *x509.Certificate, name []pkix.AttributeTypeAndValue) bool {
	if c.oid == nil {
		return c.matchesSAN(cert)
	}
	// match_name_criteria: the attribute must occur exactly once.
	var entry string
	found := 0
	for _, atv := range name {
		if atv.Type.Equal(c.oid) {
			found++
			s, ok := atv.Value.(string)
			if !ok {
				return false
			}
			entry = s
		}
	}
	return found == 1 && c.matchEntry(entry)
}

// matchesSAN is match_extension_criteria (TLSConfig.actor.cpp:663-717). Its
// switch falls through, so a DNS criterion also reads an email SAN and an IP
// criterion a URI SAN; an iPAddress SAN never converts to UTF-8 and never matches.
func (c *criterion) matchesSAN(cert *x509.Certificate) bool {
	gen, val, ok := strings.Cut(c.value, ":")
	if !ok {
		return false
	}
	want := criterion{value: val, match: c.match}
	for _, san := range rawSANs(cert) {
		var ok bool
		switch san.tag {
		case sanEmail:
			ok = gen == "EMAIL" || gen == "DNS"
		case sanDNS:
			ok = gen == "DNS"
		case sanURI:
			ok = gen == "URI" || gen == "IP"
		}
		if ok && want.matchEntry(san.value) {
			return true
		}
	}
	return false
}

// matchEntry is match_criteria_entry (TLSConfig.actor.cpp:613-646): the criterion's
// bytes are read as Latin-1 (an IA5String converted to UTF-8) and compared with the
// UTF-8 entry; an empty criterion or entry matches nothing.
func (c *criterion) matchEntry(entry string) bool {
	if entry == "" || c.value == "" {
		return false
	}
	want := latin1ToUTF8(c.value)
	switch c.match {
	case matchPrefix:
		return strings.HasPrefix(entry, want)
	case matchSuffix:
		return strings.HasSuffix(entry, want)
	}
	return entry == want
}

func latin1ToUTF8(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			b := make([]byte, 0, len(s)+1)
			for j := 0; j < len(s); j++ {
				b = utf8.AppendRune(b, rune(s[j]))
			}
			return string(b)
		}
	}
	return s
}

const (
	sanEmail = 1 // rfc822Name
	sanDNS   = 2 // dNSName
	sanURI   = 6 // uniformResourceIdentifier
)

type rawSAN struct {
	tag   int
	value string
}

var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// rawSANs returns the certificate's email, DNS and URI SANs as written; the
// parser already refused a certificate with a malformed or repeated SAN extension.
func rawSANs(cert *x509.Certificate) []rawSAN {
	var sans []rawSAN
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidSubjectAltName) {
			continue
		}
		var seq asn1.RawValue
		if _, err := asn1.Unmarshal(ext.Value, &seq); err != nil {
			return nil
		}
		for rest := seq.Bytes; len(rest) > 0; {
			var v asn1.RawValue
			var err error
			if rest, err = asn1.Unmarshal(rest, &v); err != nil {
				return nil
			}
			if v.Class == asn1.ClassContextSpecific && (v.Tag == sanEmail || v.Tag == sanDNS || v.Tag == sanURI) {
				sans = append(sans, rawSAN{tag: v.Tag, value: string(v.Bytes)})
			}
		}
	}
	return sans
}
