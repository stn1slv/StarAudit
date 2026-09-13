package signature

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stn1slv/staraudit/v2/pkg/trust"
)

// generateKeyPair returns a fresh PEM encoded RSA key pair, in the encodings
// that signReport and Check expect.
func generateKeyPair(t *testing.T) (privatePEM, publicPEM string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	privatePEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))

	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	publicPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicDER,
	}))

	return privatePEM, publicPEM
}

func testReport() *trust.Report {
	return &trust.Report{
		Factors: map[trust.FactorName]trust.Factor{
			trust.ContributionScoreFactor: {Value: 12778, TrustPercent: 0.68},
			trust.Overall:                 {TrustPercent: 0.76},
		},
	}
}

func TestEnabled(t *testing.T) {
	t.Setenv(privateKeyEnvVar, "")
	assert.False(t, Enabled(), "signing must be off when no key is configured")

	t.Setenv(privateKeyEnvVar, "-----BEGIN RSA PRIVATE KEY-----")
	assert.True(t, Enabled())
}

// TestSignReportWithoutKey covers the opt-in behaviour: with no key configured
// there is nothing to sign, and the caller must be told so explicitly rather
// than through a failure deep inside the PEM decoder.
func TestSignReportWithoutKey(t *testing.T) {
	t.Setenv(privateKeyEnvVar, "")

	_, err := signReport(testReport())

	require.Error(t, err)
	assert.Contains(t, err.Error(), privateKeyEnvVar)
}

func TestSignReportWithInvalidKey(t *testing.T) {
	t.Setenv(privateKeyEnvVar, "definitely not a PEM block")

	_, err := signReport(testReport())

	require.Error(t, err)
}

func TestSignAndCheckRoundTrip(t *testing.T) {
	privatePEM, publicPEM := generateKeyPair(t)
	t.Setenv(privateKeyEnvVar, privatePEM)
	t.Setenv(publicKeyEnvVar, publicPEM)

	report := testReport()

	sig, err := signReport(report)
	require.NoError(t, err)
	require.NotEmpty(t, sig)

	err = Check(&SignedReport{
		Report:          report,
		RepositoryOwner: "stn1slv",
		RepositoryName:  "staraudit",
		Signature:       sig,
	})

	assert.NoError(t, err)
}

// TestCheckDetectsTamperedReport verifies that the trust values themselves are
// covered by the signature.
//
// Note that RepositoryOwner and RepositoryName are deliberately not asserted
// here: signReport only hashes the trust report, so those two fields travel
// unauthenticated. That is a known gap, tracked separately from this change.
func TestCheckDetectsTamperedReport(t *testing.T) {
	privatePEM, publicPEM := generateKeyPair(t)
	t.Setenv(privateKeyEnvVar, privatePEM)
	t.Setenv(publicKeyEnvVar, publicPEM)

	report := testReport()

	sig, err := signReport(report)
	require.NoError(t, err)

	// Inflate the overall trust after signing.
	report.Factors[trust.Overall] = trust.Factor{TrustPercent: 0.99}

	err = Check(&SignedReport{Report: report, Signature: sig})

	require.Error(t, err, "a modified report must not verify")
}

func TestCheckWithWrongKey(t *testing.T) {
	privatePEM, _ := generateKeyPair(t)
	_, otherPublicPEM := generateKeyPair(t)

	t.Setenv(privateKeyEnvVar, privatePEM)
	t.Setenv(publicKeyEnvVar, otherPublicPEM)

	report := testReport()

	sig, err := signReport(report)
	require.NoError(t, err)

	err = Check(&SignedReport{Report: report, Signature: sig})

	require.Error(t, err, "a report signed with another key must not verify")
}

func TestCheckWithoutKey(t *testing.T) {
	t.Setenv(publicKeyEnvVar, "")

	err := Check(&SignedReport{Report: testReport()})

	require.Error(t, err)
	assert.Contains(t, err.Error(), publicKeyEnvVar)
}
