package auth

import (
	"testing"

	"github.com/amoylab/unla/internal/common/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestValidateAPIKey(t *testing.T) {
	a, err := NewAuth(zap.NewNop(), config.AuthConfig{
		APIKeys: []config.APIKeyConfig{
			{Name: "opencode", Key: "key-one"},
			{Name: "openwebui", Key: "key-two"},
			{Name: "empty", Key: ""},
		},
	})
	require.NoError(t, err)

	assert.NoError(t, a.ValidateAPIKey("key-one"))
	assert.NoError(t, a.ValidateAPIKey("key-two"))

	assert.ErrorIs(t, a.ValidateAPIKey("nope"), ErrInvalidAPIKey)
	assert.ErrorIs(t, a.ValidateAPIKey(""), ErrInvalidAPIKey)
	assert.ErrorIs(t, a.ValidateAPIKey("key-one "), ErrInvalidAPIKey)
}

func TestValidateAPIKeyWithoutKeys(t *testing.T) {
	a, err := NewAuth(zap.NewNop(), config.AuthConfig{})
	require.NoError(t, err)

	assert.ErrorIs(t, a.ValidateAPIKey("anything"), ErrInvalidAPIKey)
}
