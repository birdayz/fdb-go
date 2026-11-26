//go:build !fdbdocker

package simfdb

// DockerRequired is false without the `fdbdocker` build tag: a missing container takes the
// sanctioned skip. See differential_docker_required.go for why the tagged build exists.
const DockerRequired = false
