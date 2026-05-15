package container

import "os"

// PrepareChatResumeForTest exposes the private prepareChatResume method so
// tests in the container package can call it directly without going through
// StartChat. The shim matches the private signature exactly.
func (m *Manager) PrepareChatResumeForTest(containerName, sessionID, project string, resume *ChatResume) (chatResumeDelivery, error) {
	return m.prepareChatResume(containerName, sessionID, project, resume)
}

// SetCreateFileForTest overrides the createFile hook for tests. Allows tests
// to inject file-creation errors or simulate write failures.
func (m *Manager) SetCreateFileForTest(fn func(path string) (*os.File, error)) {
	m.createFile = fn
}

// SecretsDirForTest returns the configured secrets directory for testing.
func (m *Manager) SecretsDirForTest() string {
	return m.cfg.SecretsDir
}
