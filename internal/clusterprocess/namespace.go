package clusterprocess

import (
	"errors"
	"regexp"
)

var namespaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

// NamespaceCommand uses ip's exec (no sudo wrapper), preserving the child PID
// and process group that readiness and crash injection already track.
func NamespaceCommand(namespace string, args ...string) ([]string, error) {
	if !namespaceName.MatchString(namespace) || len(args) == 0 || args[0] == "" {
		return nil, errors.New("namespace launch requires a safe namespace name and executable")
	}
	return append([]string{"ip", "netns", "exec", namespace}, args...), nil
}

func (m *Manager) StartInNamespace(dir, name, namespace string, env []string, args ...string) (*Child, error) {
	command, err := NamespaceCommand(namespace, args...)
	if err != nil {
		return nil, err
	}
	return m.Start(dir, name, env, command...)
}
