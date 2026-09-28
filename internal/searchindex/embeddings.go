package searchindex

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	BatchSize() int
}

// EmbeddingSettings is the daemon's resolved embedding configuration.
type EmbeddingSettings struct {
	BaseURL             string
	Model               string
	APIKey              string
	Salt                string
	RecipeVersion       int
	Dims                int
	BatchSize           int
	Timeout             time.Duration
	InputTypeMode       string
	TrustPrivateNetwork bool
}

// Embeddings is the kit embedding client bound to its vector-space identity.
type Embeddings struct {
	client    *embedclient.Client
	space     embedmodel.Descriptor
	batchSize int
}

// NewEmbeddings validates settings and constructs an origin-pinned client.
// Client retries stay off; the reconciler owns backoff between turns.
func NewEmbeddings(settings EmbeddingSettings) (*Embeddings, error) {
	if err := checkPlaintextHost(settings.BaseURL, settings.TrustPrivateNetwork); err != nil {
		return nil, err
	}
	space := embedmodel.Descriptor{
		Model: embedconfig.Model{
			Name:          settings.Model,
			Revision:      settings.Salt,
			Dimensions:    settings.Dims,
			Metric:        embedconfig.MetricCosine,
			Normalization: embedconfig.NormalizationL2,
		},
		Roles: embedconfig.Roles{
			// The rendered review document is the document formatter, so a
			// recipe change starts a new generation.
			DocumentFormatter: "roborev.searchdoc/v" + strconv.Itoa(settings.RecipeVersion),
			InputType:         embedconfig.InputType(settings.InputTypeMode),
		},
		Deployment: embedconfig.Deployment{
			BaseURL:             settings.BaseURL,
			PinEndpoint:         true,
			TrustPrivateNetwork: settings.TrustPrivateNetwork,
		},
	}
	client, err := embedclient.New(embedclient.Options{
		Model:      space.Model,
		Roles:      space.Roles,
		Deployment: space.Deployment,
		Batch:      embedconfig.Batch{Items: settings.BatchSize},
		Transport:  embedconfig.Transport{Timeout: settings.Timeout},
		APIKey:     settings.APIKey,
	})
	if err != nil {
		return nil, err
	}
	legacy, err := legacyFingerprint(settings)
	if err != nil {
		return nil, err
	}
	space.Legacy = []string{legacy}
	if err := space.Validate(); err != nil {
		return nil, err
	}
	batchSize := settings.BatchSize
	if batchSize <= 0 {
		batchSize = embedconfig.DefaultBatchItems
	}
	return &Embeddings{client: client, space: space, batchSize: batchSize}, nil
}

// EncodeFunc sends texts for role unchanged.
func (e *Embeddings) EncodeFunc(role embedconfig.Role) vector.EncodeFunc {
	return e.client.EncodeFunc(role)
}

// Space returns the vector-space descriptor.
func (e *Embeddings) Space() embedmodel.Descriptor { return e.space }

// BatchSize returns the maximum number of inputs sent in one provider request.
func (e *Embeddings) BatchSize() int { return e.batchSize }

// legacyFingerprint is the generation fingerprint that releases before the
// kit client stored. Existing vectors keep serving under it.
func legacyFingerprint(settings EmbeddingSettings) (string, error) {
	endpoint, err := embedconfig.CanonicalEndpoint(settings.BaseURL, settings.TrustPrivateNetwork)
	if err != nil {
		return "", err
	}
	mode := settings.InputTypeMode
	if mode == "" {
		mode = string(embedconfig.InputTypeNone)
	}
	params := map[string]string{
		"endpoint":        endpoint,
		"input_type_mode": mode,
		"recipe":          strconv.Itoa(settings.RecipeVersion),
	}
	if settings.Salt != "" {
		params["salt"] = settings.Salt
	}
	return vector.Generation{Model: settings.Model, Dimensions: settings.Dims, Params: params}.Fingerprint(), nil
}

// checkPlaintextHost keeps trust_private_network limited to IP literals.
// Kit's policy also admits host names under that setting, and a name can
// resolve to a public address.
func checkPlaintextHost(baseURL string, trustPrivateNetwork bool) error {
	if !trustPrivateNetwork {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") {
		return nil
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return nil
	}
	return errors.New("embedding: plaintext HTTP endpoint requires loopback or trusted private network address")
}

// embeddingAPIError returns the provider status error carried by err.
func embeddingAPIError(err error) (*embedclient.APIError, bool) {
	return errors.AsType[*embedclient.APIError](err)
}

// embeddingDefinitive reports a provider rejection that retrying cannot fix
// without operator action or different input.
func embeddingDefinitive(apiErr *embedclient.APIError) bool {
	return apiErr.InputRejected() || apiErr.CredentialsRejected() || apiErr.StatusCode == http.StatusNotFound
}

var _ Embedder = (*Embeddings)(nil)
