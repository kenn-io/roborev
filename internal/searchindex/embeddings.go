package searchindex

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

// Embedder encodes review documents and search queries for one vector space.
type Embedder interface {
	EncodeFunc(role embedconfig.Role) vector.EncodeFunc
	// Space identifies the vector space, including the fingerprints of
	// generations that earlier releases stored for the same configuration.
	Space() embedmodel.Descriptor
	Batch() embedconfig.Batch
}

// Embeddings is the kit embedding client bound to its vector-space identity.
type Embeddings struct {
	client *embedclient.Client
	space  embedmodel.Descriptor
	batch  embedconfig.Batch
}

// NewEmbeddings builds the client from kit's standard embedder settings and
// the resolved API key. recipeVersion identifies how review documents are
// rendered. Client retries stay off; the reconciler owns backoff between
// turns.
func NewEmbeddings(config embedconfig.Embedder, apiKey string, recipeVersion int) (*Embeddings, error) {
	parts, err := config.Parts()
	if err != nil {
		return nil, err
	}
	// Vectors are keyed by endpoint, and the rendered review document is the
	// document formatter, so changing either starts a new generation.
	parts.Deployment.PinEndpoint = true
	parts.Roles.DocumentFormatter = "roborev.searchdoc/v" + strconv.Itoa(recipeVersion)
	client, err := embedclient.New(embedclient.Options{
		Model:      parts.Model,
		Roles:      parts.Roles,
		Deployment: parts.Deployment,
		Batch:      parts.Batch,
		Transport:  parts.Transport,
		APIKey:     apiKey,
	})
	if err != nil {
		return nil, err
	}
	legacy, err := legacyFingerprint(config, recipeVersion)
	if err != nil {
		return nil, err
	}
	space := embedmodel.Descriptor{
		Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment,
		Legacy: []string{legacy},
	}
	if err := space.Validate(); err != nil {
		return nil, err
	}
	return &Embeddings{client: client, space: space, batch: parts.Batch}, nil
}

// EncodeFunc sends texts for role unchanged.
func (e *Embeddings) EncodeFunc(role embedconfig.Role) vector.EncodeFunc {
	return e.client.EncodeFunc(role)
}

// Space returns the vector-space descriptor.
func (e *Embeddings) Space() embedmodel.Descriptor { return e.space }

// Batch returns the per-request input limits.
func (e *Embeddings) Batch() embedconfig.Batch { return e.batch }

// legacyFingerprint is the generation fingerprint that releases before the
// kit client stored. Existing vectors keep serving under it.
func legacyFingerprint(config embedconfig.Embedder, recipeVersion int) (string, error) {
	endpoint, err := embedconfig.CanonicalEndpoint(config.BaseURL, config.TrustPrivateNetwork)
	if err != nil {
		return "", err
	}
	mode := strings.TrimSpace(config.InputTypeMode)
	if mode == "" {
		mode = string(embedconfig.InputTypeNone)
	}
	params := map[string]string{
		"endpoint":        endpoint,
		"input_type_mode": mode,
		"recipe":          strconv.Itoa(recipeVersion),
	}
	if config.FingerprintSalt != "" {
		params["salt"] = config.FingerprintSalt
	}
	return vector.Generation{Model: config.Model, Dimensions: config.Dims, Params: params}.Fingerprint(), nil
}

// embeddingAPIError returns the provider status error carried by err.
func embeddingAPIError(err error) (*embedclient.APIError, bool) {
	return errors.AsType[*embedclient.APIError](err)
}

// embeddingDefinitive reports a provider rejection that retrying cannot fix
// without operator action or different input. Kit classifies a wrong route
// (404), an unknown model, or unsupported dimensions as an invalid request.
// A 400 Kit cannot classify is still a refused request, so it waits the full
// backoff instead of retrying quickly.
func embeddingDefinitive(apiErr *embedclient.APIError) bool {
	return apiErr.InputRejected() || apiErr.CredentialsRejected() ||
		apiErr.Reason == embedclient.ReasonInvalidRequest || apiErr.StatusCode == http.StatusBadRequest
}

var _ Embedder = (*Embeddings)(nil)
