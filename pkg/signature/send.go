package signature

import (
	"bytes"
	stdcontext "context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"

	"github.com/Ullaakut/disgo"
	staraudit_context "github.com/stn1slv/staraudit/v2/pkg/context"
	"github.com/stn1slv/staraudit/v2/pkg/trust"
)

// SignedReport represents a report that has been signed
// by a legitimate version of StarAudit.
type SignedReport struct {
	*trust.Report

	RepositoryOwner string
	RepositoryName  string

	Signature []byte
}

const (
	// privateKeyEnvVar holds the PEM encoded RSA key used to sign reports.
	privateKeyEnvVar = "STARAUDIT_PRIVATE_KEY"

	// publicKeyEnvVar holds the PEM encoded RSA key used to verify reports.
	publicKeyEnvVar = "STARAUDIT_PUBLIC_KEY"
)

// Enabled reports whether a signing key is configured.
//
// Signing is opt-in: no key is embedded in the binary, so reports are only
// signed and uploaded when one is supplied through the environment.
func Enabled() bool {
	return os.Getenv(privateKeyEnvVar) != ""
}

// SendReport signs a report and sends it to Astrolab.
func SendReport(ctx stdcontext.Context, starauditCtx *staraudit_context.Context, report *trust.Report) error {
	signature, err := signReport(report)
	if err != nil {
		return err
	}

	return sendReport(ctx, SignedReport{
		Report:          report,
		RepositoryOwner: starauditCtx.RepoOwner,
		RepositoryName:  starauditCtx.RepoName,
		Signature:       signature,
	})
}

func signReport(report *trust.Report) ([]byte, error) {
	data, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal trust report: %w", err)
	}

	hashedReport := sha512.Sum512(data)

	pemKey := os.Getenv(privateKeyEnvVar)
	if pemKey == "" {
		return nil, fmt.Errorf("no signing key: set %s to sign and upload reports", privateKeyEnvVar)
	}

	keyBlock, _ := pem.Decode([]byte(pemKey))
	if keyBlock == nil {
		return nil, fmt.Errorf("unable to decode private key")
	}

	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unable to parse private key: %w", err)
	}

	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA512, hashedReport[:])
	if err != nil {
		return nil, fmt.Errorf("unable to sign trust report: %w", err)
	}

	return signature, nil
}

func sendReport(ctx stdcontext.Context, report SignedReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("unable to marshal signed report: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://astronomer.ullaakut.eu", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("unable to prepare request to staraudit server: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unable to send signed report to staraudit server: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != 201 {
		return fmt.Errorf("staraudit server did not trust this report: %v", response.Status)
	}

	disgo.Debugln("Signed report successfully sent to staraudit server, thanks for your contribution!")

	return nil
}
