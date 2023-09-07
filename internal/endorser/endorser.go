// Copyright 2022-2023 The Project Oak Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package endorser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"go.uber.org/multierr"

	"github.com/project-oak/transparent-release/internal/model"
	"github.com/project-oak/transparent-release/internal/verifier"
	"github.com/project-oak/transparent-release/pkg/claims"
	"github.com/project-oak/transparent-release/pkg/intoto"
	pb "github.com/project-oak/transparent-release/pkg/proto/verifier"
)

// ParsedProvenance contains a provenance in the internal ProvenanceIR format,
// and metadata about the source of the provenance. In case of a provenance
// wrapped in a DSSE envelope, `SourceMetadata` contains the URI and digest of
// the DSSE document, while `Provenance` contains the provenance itself.
type ParsedProvenance struct {
	Provenance     model.ProvenanceIR
	SourceMetadata claims.ProvenanceData
}

// GenerateEndorsement generates an endorsement statement for the given binary
// and validity duration, using the given provenances as evidence and
// user-specified VerificationOptions to verify them.
func GenerateEndorsement(binaryName string, digests intoto.DigestSet, verOpts *pb.VerificationOptions, validityDuration claims.ClaimValidity, provenances []ParsedProvenance) (*intoto.Statement, error) {
	provenanceIRs := make([]model.ProvenanceIR, 0, len(provenances))
	provenancesData := make([]claims.ProvenanceData, 0, len(provenances))
	for _, p := range provenances {
		provenanceIRs = append(provenanceIRs, p.Provenance)
		provenancesData = append(provenancesData, p.SourceMetadata)
	}

	// First verify the non-negiotiable: binary name and digest.
	err := verifier.Verify(provenanceIRs, &pb.VerificationOptions{
		AllWithBinaryName: &pb.VerifyAllWithBinaryName{BinaryName: binaryName},
		AllWithBinaryDigests: &pb.VerifyAllWithBinaryDigests{
			Formats: []string{"sha2-256"},
			Digests: []string{digests["sha2-256"]},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to verify provenances: %v", err)
	}

	// Additionally, verify any aspects requested by the caller.
	err = verifier.Verify(provenanceIRs, verOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to verify provenances: %v", err)
	}

	verifiedProvenances := claims.VerifiedProvenanceSet{
		Digests:     digests,
		BinaryName:  binaryName,
		Provenances: provenancesData,
	}

	return claims.GenerateEndorsementStatement(validityDuration, verifiedProvenances), nil
}

// LoadProvenances loads a number of provenance from the give URIs. Returns an
// array of ParsedProvenance instances, or an error if loading or parsing any
// of the provenances fails. See LoadProvenance for more details.
func LoadProvenances(provenanceURIs []string) ([]ParsedProvenance, error) {
	provenances := make([]ParsedProvenance, 0, len(provenanceURIs))
	for _, uri := range provenanceURIs {
		parsedProvenance, err := LoadProvenance(uri)
		if err != nil {
			return nil, fmt.Errorf("couldn't load the provenance from %s: %v", uri, err)
		}
		provenances = append(provenances, *parsedProvenance)
	}
	return provenances, nil
}

// LoadProvenance loads a provenance from the give URI (either a local file or
// a remote file on an HTTP/HTTPS server). Returns an instance of
// ParsedProvenance if loading and parsing is successful, or an error Otherwise.
func LoadProvenance(provenanceURI string) (*ParsedProvenance, error) {
	provenanceBytes, err := GetProvenanceBytes(provenanceURI)
	if err != nil {
		return nil, fmt.Errorf("couldn't load the provenance bytes from %s: %v", provenanceURI, err)
	}

	// Parse into a validated provenance to get the predicate/build type of the provenance.
	var errs error
	validatedProvenance, err := model.ParseStatementData(provenanceBytes)
	if err != nil {
		errs = multierr.Append(errs, fmt.Errorf("parsing bytes as an in-toto statement: %v", err))
		validatedProvenance, err = model.ParseEnvelope(provenanceBytes)
		if err != nil {
			errs = multierr.Append(errs, fmt.Errorf("parsing bytes as a DSSE envelop: %v", err))
			return nil, fmt.Errorf("couldn't parse bytes from %s into a validated provenance: %v", provenanceURI, errs)
		}
	}

	// Map to internal provenance representation based on the predicate/build type.
	provenanceIR, err := model.FromValidatedProvenance(validatedProvenance)
	if err != nil {
		return nil, fmt.Errorf("couldn't map from %s to internal representation: %v", validatedProvenance, err)
	}
	sum256 := sha256.Sum256(provenanceBytes)
	return &ParsedProvenance{
		Provenance: *provenanceIR,
		SourceMetadata: claims.ProvenanceData{
			URI:          provenanceURI,
			SHA256Digest: hex.EncodeToString(sum256[:]),
		},
	}, nil
}

// GetProvenanceBytes fetches provenance bytes from the give URI. Supported URI
// schemes are "http", "https", and "file". Only local files are supported.
func GetProvenanceBytes(provenanceURI string) ([]byte, error) {
	uri, err := url.Parse(provenanceURI)
	if err != nil {
		return nil, fmt.Errorf("could not parse the URI (%q): %v", provenanceURI, err)
	}

	if uri.Scheme == "http" || uri.Scheme == "https" {
		return getJSONOverHTTP(provenanceURI)
	} else if uri.Scheme == "file" {
		return getLocalJSONFile(uri)
	}

	return nil, fmt.Errorf("unsupported URI scheme (%q)", uri.Scheme)
}

func getJSONOverHTTP(uri string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create HTTP request: %v", err)
	}

	req.Header.Set("Accept", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not receive response from server: %v", err)
	}

	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func getLocalJSONFile(uri *url.URL) ([]byte, error) {
	if uri.Host != "" {
		return nil, fmt.Errorf("invalid scheme (%q) and host (%q) combination", uri.Scheme, uri.Host)
	}
	if _, err := os.Stat(uri.Path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%q does not exist", uri.Path)
	}
	return os.ReadFile(uri.Path)
}
