package build

import "context"

// Interface for an extern secrets service provider
type SecretFetcher interface {
	GetSecret(ctx context.Context, source string) (string, error) // Must return the secret value
}

func (s *BuildService) GetSecret(ctx context.Context, source string) (string, error) {
	s.mutex.Lock()
	fetcher := s.secretFetcher
	defer s.mutex.Unlock()

	if fetcher == nil {
		// Using the default DummySecretFetcher if no fetcher is initialized
		fetcher = &DummySecretFetcher{}
	}
	return fetcher.GetSecret(ctx, source)
}