//go:build fdbdocker

package simfdb

// DockerRequired reports whether a missing FDB container is a FAILURE rather than a skip.
//
// The differential against a real cluster is the ONLY evidence SimFDB behaves like FDB. Every
// other test in this package asks SimFDB whether SimFDB is right. So a Dockerless run that
// quietly skips it produces a fully green suite that has verified nothing about fidelity — and
// green-with-nothing-verified is the exact failure mode the sim exists to prevent elsewhere.
//
// The `fdbdocker` build tag makes the requirement explicit: CI builds with it and a missing
// container is red. A laptop without Docker builds without it and takes the sanctioned skip.
// The point is that "the differential ran" becomes a property of the BUILD, not an accident of
// the environment.
const DockerRequired = true
