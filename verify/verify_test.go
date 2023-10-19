// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package verify

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-sev-guest/abi"
	sg "github.com/google/go-sev-guest/client"
	"github.com/google/go-sev-guest/kds"
	pb "github.com/google/go-sev-guest/proto/sevsnp"
	test "github.com/google/go-sev-guest/testing"
	testclient "github.com/google/go-sev-guest/testing/client"
	"github.com/google/go-sev-guest/verify/testdata"
	"github.com/google/go-sev-guest/verify/trust"
	"github.com/google/logger"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var (
	signMu sync.Once
	signer *test.AmdSigner
)

func product() string {
	parts := strings.SplitN(test.GetProductName(), "-", 2)
	return parts[0]
}

func initSigner() {

	newSigner, err := test.DefaultTestOnlyCertChain(product(), time.Now())
	if err != nil { // Unexpected
		panic(err)
	}
	signer = newSigner
}

func TestMain(m *testing.M) {
	logger.Init("VerifyTestLog", false, false, os.Stderr)
	os.Exit(m.Run())
}

func testProduct(t testing.TB) *pb.SevProduct {
	result, err := kds.ParseProductName(test.GetProductName(), abi.VcekReportSigner)
	if err != nil {
		t.Errorf("bad product flag %q: %v", test.GetProductName(), err)
	}
	return result
}

func TestEmbeddedCertsAppendixB3Expectations(t *testing.T) {
	// https://www.amd.com/system/files/TechDocs/55766_SEV-KM_API_Specification.pdf
	// Appendix B.1
	for _, root := range trust.DefaultRootCerts {
		if err := validateAskSev(root); err != nil {
			t.Errorf("Embedded ASK failed validation: %v", err)
		}
		if err := validateArkSev(root); err != nil {
			t.Errorf("Embedded ARK failed validation: %v", err)
		}
	}
}

func TestFakeCertsKDSExpectations(t *testing.T) {
	signMu.Do(initSigner)
	trust.ClearProductCertCache()
	root := &trust.AMDRootCerts{
		Product: product(),
		ProductCerts: &trust.ProductCerts{
			Ark: signer.Ark,
			Ask: signer.Ask,
		},
		// No ArkSev or AskSev intentionally for test certs.
	}
	if err := validateArkX509(root); err != nil {
		t.Errorf("fake ARK validation error: %v", err)
	}
	if err := validateAskX509(root); err != nil {
		t.Errorf("fake ASK validation error: %v", err)
	}
}

func TestParseVcekCert(t *testing.T) {
	cert, err := x509.ParseCertificate(testdata.VcekBytes)
	if err != nil {
		t.Errorf("could not parse valid VCEK certificate: %v", err)
	}
	if _, err := validateKDSCertificateProductNonspecific(cert, abi.VcekReportSigner); err != nil {
		t.Errorf("could not validate valid VCEK certificate: %v", err)
	}
}

func TestVerifyVcekCert(t *testing.T) {
	// This certificate is committed regardless of its expiration date, but we'll adjust the
	// CurrentTime to compare against so that the validity with respect to time is always true.
	root := new(trust.AMDRootCerts)
	if err := root.FromKDSCertBytes(testdata.MilanVcekBytes); err != nil {
		t.Fatalf("could not read Milan certificate file: %v", err)
	}
	vcek, err := x509.ParseCertificate(testdata.VcekBytes)
	if err != nil {
		t.Errorf("could not parse valid VCEK certificate: %v", err)
	}
	now := time.Date(2022, time.September, 24, 1, 0, 0, 0, time.UTC)
	opts := root.X509Options(now, abi.VcekReportSigner)
	if opts == nil {
		t.Fatalf("root x509 certificates missing: %v", root)
		return
	}
	// This time is within the 25 year lifespan of the Milan product.
	chains, err := vcek.Verify(*opts)
	if err != nil {
		t.Errorf("could not verify VCEK certificate: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("x509 verification returned %d chains, want 1", len(chains))
	}
	if len(chains[0]) != 3 {
		t.Fatalf("x509 verification returned a chain of length %d, want length 3", len(chains[0]))
	}
	if !chains[0][0].Equal(vcek) {
		t.Errorf("VCEK verification chain did not start with the VCEK certificate: %v", chains[0][0])
	}
	if !chains[0][1].Equal(root.ProductCerts.Ask) {
		t.Errorf("VCEK verification chain did not step to with the ASK certificate: %v", chains[0][1])
	}
	if !chains[0][2].Equal(root.ProductCerts.Ark) {
		t.Errorf("VCEK verification chain did not end with the ARK certificate: %v", chains[0][2])
	}
}

func TestSnpReportSignature(t *testing.T) {
	tests := test.TestCases()
	now := time.Date(2022, time.May, 3, 9, 0, 0, 0, time.UTC)
	d, err := test.TcDevice(tests, &test.DeviceOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Open("/dev/sev-guest"); err != nil {
		t.Error(err)
	}
	defer d.Close()
	for _, tc := range tests {
		if testclient.SkipUnmockableTestCase(&tc) {
			continue
		}
		// Does the Raw report match expectations?
		raw, err := sg.GetRawReport(d, tc.Input)
		if !test.Match(err, tc.WantErr) {
			t.Fatalf("GetRawReport(d, %v) = %v, %v. Want err: %v", tc.Input, raw, err, tc.WantErr)
		}
		if tc.WantErr == "" {
			got := abi.SignedComponent(raw)
			want := abi.SignedComponent(tc.Output[:])
			if !bytes.Equal(got, want) {
				t.Errorf("%s: GetRawReport(%v) = %v, want %v", tc.Name, tc.Input, got, want)
			}
			key := d.Signer.Vcek
			if tc.EK == test.KeyChoiceVlek {
				key = d.Signer.Vlek
			}
			if err := SnpReportSignature(raw, key); err != nil {
				t.Errorf("signature with test keys did not verify: %v", err)
			}
		}
	}
}

func TestKdsMetadataLogic(t *testing.T) {
	signMu.Do(initSigner)
	trust.ClearProductCertCache()
	asn1Zero, _ := asn1.Marshal(0)
	productName, _ := asn1.MarshalWithParams("Cookie-B0", "ia5")
	var hwid [64]byte
	asn1Hwid, _ := asn1.Marshal(hwid[:])
	tests := []struct {
		name    string
		builder test.AmdSignerBuilder
		wantErr string
	}{
		{
			name:    "no error",
			builder: test.AmdSignerBuilder{Keys: signer.Keys},
		},
		{
			name: "ARK issuer country",
			builder: test.AmdSignerBuilder{
				Keys: signer.Keys,
				ArkCustom: test.CertOverride{
					Issuer:  &pkix.Name{Country: []string{"Canada"}},
					Subject: &pkix.Name{Country: []string{"Canada"}},
				},
			},
			wantErr: "country 'Canada' not expected for AMD. Expected 'US'",
		},
		{
			name: "ARK wrong CRL",
			builder: test.AmdSignerBuilder{
				Keys: signer.Keys,
				ArkCustom: test.CertOverride{
					CRLDistributionPoints: []string{"http://example.com"},
				},
			},
			wantErr: fmt.Sprintf("ARK CRL distribution point is 'http://example.com', want 'https://kdsintf.amd.com/vcek/v1/%s/crl'", product()),
		},
		{
			name: "ARK too many CRLs",
			builder: test.AmdSignerBuilder{
				Keys: signer.Keys,
				ArkCustom: test.CertOverride{
					CRLDistributionPoints: []string{fmt.Sprintf("https://kdsintf.amd.com/vcek/v1/%s/crl", product()), "http://example.com"},
				},
			},
			wantErr: "ARK has 2 CRL distribution points, want 1",
		},
		{
			name: "ASK subject state",
			builder: test.AmdSignerBuilder{
				Keys: signer.Keys,
				ArkCustom: test.CertOverride{
					Subject: &pkix.Name{
						Country:  []string{"US"},
						Locality: []string{"Santa Clara"},
						Province: []string{"TX"},
					},
				},
			},
			wantErr: "state 'TX' not expected for AMD. Expected 'CA'",
		},
		{
			name: "VCEK unknown product",
			builder: test.AmdSignerBuilder{
				Keys: signer.Keys,
				VcekCustom: test.CertOverride{
					Extensions: []pkix.Extension{
						{
							Id:    kds.OidStructVersion,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidProductName1,
							Value: productName,
						},
						{
							Id:    kds.OidBlSpl,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidTeeSpl,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidSnpSpl,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidSpl4,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidSpl5,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidSpl6,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidSpl7,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidUcodeSpl,
							Value: asn1Zero,
						},
						{
							Id:    kds.OidHwid,
							Value: asn1Hwid,
						},
					},
				},
			},
			wantErr: "unknown product",
		},
	}
	for _, tc := range tests {
		bcopy := tc.builder
		newSigner, err := (&bcopy).TestOnlyCertChain()
		if err != nil {
			t.Errorf("%+v.TestOnlyCertChain() errored unexpectedly: %v", tc.builder, err)
			continue
		}
		// Trust the test-generated root if the test should pass. Otherwise, other root logic
		// won't get tested.
		options := &Options{
			TrustedRoots: map[string][]*trust.AMDRootCerts{
				product(): {&trust.AMDRootCerts{
					Product: product(),
					ProductCerts: &trust.ProductCerts{
						Ark: newSigner.Ark,
						Ask: newSigner.Ask,
					},
				}},
			},
			Now:     time.Date(1, time.January, 5, 0, 0, 0, 0, time.UTC),
			Product: testProduct(t),
		}
		if tc.wantErr != "" {
			options = &Options{Product: testProduct(t)}
		}
		vcekPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: newSigner.Vcek.Raw})
		vcek, _, err := decodeCerts(&pb.CertificateChain{VcekCert: vcekPem, AskCert: newSigner.Ask.Raw, ArkCert: newSigner.Ark.Raw}, abi.VcekReportSigner, options)
		if !test.Match(err, tc.wantErr) {
			t.Errorf("%s: decodeCerts(...) = %+v, %v did not error as expected. Want %q", tc.name, vcek, err, tc.wantErr)
		}
	}
}

func TestCRLRootValidity(t *testing.T) {
	// Tests that the CRL is signed by the ARK.
	signMu.Do(initSigner)
	trust.ClearProductCertCache()
	now := time.Date(2022, time.June, 14, 12, 0, 0, 0, time.UTC)

	ark2, err := test.DefaultArk()
	if err != nil {
		t.Fatal(err)
	}
	sb := &test.AmdSignerBuilder{
		Product:          product(),
		ArkCreationTime:  now,
		AskCreationTime:  now,
		VcekCreationTime: now,
		CSPID:            "go-sev-guest",
		Keys: &test.AmdKeys{
			Ark:  ark2,
			Ask:  signer.Keys.Ask,
			Asvk: signer.Keys.Asvk,
			Vcek: signer.Keys.Vcek,
			Vlek: signer.Keys.Vlek,
		},
		VcekCustom: test.CertOverride{
			SerialNumber: big.NewInt(0xd),
		},
		AskCustom: test.CertOverride{
			SerialNumber: big.NewInt(0x8088),
		},
	}
	signer2, err := sb.TestOnlyCertChain()
	if err != nil {
		t.Fatal(err)
	}

	insecureRandomness := rand.New(rand.NewSource(0xc0de))
	afterCreation := now.Add(1 * time.Minute)
	template := &x509.RevocationList{
		SignatureAlgorithm: x509.SHA384WithRSAPSS,
		RevokedCertificates: []pkix.RevokedCertificate{
			// The default fake VCEK serial number is 0.
			{SerialNumber: big.NewInt(0), RevocationTime: afterCreation},
			{SerialNumber: big.NewInt(0x8088), RevocationTime: afterCreation},
		},
		Number: big.NewInt(1),
	}
	root := &trust.AMDRootCerts{
		Product: product(),
		ProductCerts: &trust.ProductCerts{
			Ark: signer.Ark,
			Ask: signer.Ask,
		},
	}

	// Now try signing a CRL with a different root that certifies Vcek with a different serial number.
	crl, err := x509.CreateRevocationList(insecureRandomness, template, signer2.Ark, signer2.Keys.Ark)
	if err != nil {
		t.Fatal(err)
	}
	g2 := test.SimpleGetter(
		map[string][]byte{
			fmt.Sprintf("https://kdsintf.amd.com/vcek/v1/%s/crl", product()): crl,
		},
	)
	wantErr := "CRL is not signed by ARK"
	if err := VcekNotRevoked(root, signer2.Vcek, &Options{Getter: g2, Product: testProduct(t)}); !test.Match(err, wantErr) {
		t.Errorf("Bad Root: VcekNotRevoked(%v) did not error as expected. Got %v, want %v", signer.Vcek, err, wantErr)
	}

	// Finally try checking a VCEK that's signed by a revoked ASK.
	root2 := &trust.AMDRootCerts{
		Product: product(),
		ProductCerts: &trust.ProductCerts{
			Ark: signer2.Ark,
			Ask: signer2.Ask,
		},
	}
	wantErr2 := "ASK was revoked at 2022-06-14 12:01:00 +0000 UTC"
	if err := VcekNotRevoked(root2, signer2.Vcek, &Options{Getter: g2, Product: testProduct(t)}); !test.Match(err, wantErr2) {
		t.Errorf("Bad ASK: VcekNotRevoked(%v) did not error as expected. Got %v, want %v", signer.Vcek, err, wantErr2)
	}
}

func TestOpenGetExtendedReportVerifyClose(t *testing.T) {
	trust.ClearProductCertCache()
	tests := test.TestCases()
	d, goodRoots, badRoots, kds := testclient.GetSevGuest(tests, &test.DeviceOptions{Now: time.Now()}, t)
	defer d.Close()
	type reportGetter func(sg.Device, [64]byte) (*pb.Attestation, error)
	reportOnly := func(d sg.Device, input [64]byte) (*pb.Attestation, error) {
		report, err := sg.GetReport(d, input)
		if err != nil {
			return nil, err
		}
		return &pb.Attestation{Report: report}, nil
	}
	reportGetters := []struct {
		name           string
		getter         reportGetter
		skipVlek       bool
		badRootErr     string
		vlekOnly       bool
		vlekErr        string
		vlekBadRootErr string
	}{
		{
			name:           "GetExtendedReport",
			getter:         sg.GetExtendedReport,
			badRootErr:     "error verifying VCEK certificate",
			vlekBadRootErr: "error verifying VLEK certificate",
		},
		{
			name:           "GetReport",
			getter:         reportOnly,
			badRootErr:     "error verifying VCEK certificate",
			vlekErr:        "VLEK certificate is missing",
			vlekBadRootErr: "VLEK certificate is missing",
		},
		{
			name: "GetReportVlek",
			getter: func(d sg.Device, input [64]byte) (*pb.Attestation, error) {
				attestation, err := reportOnly(d, input)
				if err != nil {
					return nil, err
				}
				// If fake, we can provide the VLEK. Otherwise we have to error.
				if attestation.CertificateChain == nil {
					attestation.CertificateChain = &pb.CertificateChain{}
				}
				chain := attestation.CertificateChain
				info, _ := abi.ParseSignerInfo(attestation.GetReport().GetSignerInfo())
				if sg.UseDefaultSevGuest() && info.SigningKey == abi.VlekReportSigner {
					if td, ok := d.(*test.Device); ok {
						chain.VlekCert = td.Signer.Vlek.Raw
					}
				}
				return attestation, nil
			},
			skipVlek:       !sg.UseDefaultSevGuest(),
			vlekOnly:       true,
			badRootErr:     "error verifying VLEK certificate",
			vlekBadRootErr: "error verifying VLEK certificate",
		},
	}
	// Trust the test device's root certs.
	options := &Options{TrustedRoots: goodRoots, Getter: kds, Product: testProduct(t)}
	badOptions := &Options{TrustedRoots: badRoots, Getter: kds, Product: testProduct(t)}
	for _, tc := range tests {
		if testclient.SkipUnmockableTestCase(&tc) {
			t.Run(tc.Name, func(t *testing.T) { t.Skip() })
			continue
		}
		for _, getReport := range reportGetters {
			t.Run(tc.Name+"_"+getReport.name, func(t *testing.T) {
				if getReport.skipVlek && tc.EK == test.KeyChoiceVlek {
					t.Skip()
					return
				}
				if getReport.vlekOnly && tc.EK != test.KeyChoiceVlek {
					t.Skip()
					return
				}
				ereport, err := getReport.getter(d, tc.Input)
				if !test.Match(err, tc.WantErr) {
					t.Fatalf("(d, %v) = %v, %v. Want err: %v", tc.Input, ereport, err, tc.WantErr)
				}
				if tc.WantErr == "" {
					var wantAttestationErr string
					if tc.EK == test.KeyChoiceVlek && getReport.vlekErr != "" {
						wantAttestationErr = getReport.vlekErr
					}
					if err := SnpAttestation(ereport, options); !test.Match(err, wantAttestationErr) {
						t.Errorf("SnpAttestation(%v) = %v. Want err: %q", ereport, err, wantAttestationErr)
					}

					wantBad := getReport.badRootErr
					if tc.EK == test.KeyChoiceVlek && getReport.vlekBadRootErr != "" {
						wantBad = getReport.vlekBadRootErr
					}
					if err := SnpAttestation(ereport, badOptions); !test.Match(err, wantBad) {
						t.Errorf("SnpAttestation(_) bad root test errored unexpectedly: %v, want %s",
							err, wantBad)
					}
				}
			})
		}
	}
}

func TestRealAttestationVerification(t *testing.T) {
	trust.ClearProductCertCache()
	var nonce [64]byte
	copy(nonce[:], []byte{1, 2, 3, 4, 5})
	getter := test.SimpleGetter(
		map[string][]byte{
			"https://kdsintf.amd.com/vcek/v1/Milan/cert_chain": testdata.MilanVcekBytes,
			// Use the VCEK's hwID and known TCB values to specify the URL its VCEK cert would be fetched from.
			"https://kdsintf.amd.com/vcek/v1/Milan/3ac3fe21e13fb0990eb28a802e3fb6a29483a6b0753590c951bdd3b8e53786184ca39e359669a2b76a1936776b564ea464cdce40c05f63c9b610c5068b006b5d?blSPL=2&teeSPL=0&snpSPL=5&ucodeSPL=68": testdata.VcekBytes,
		},
	)
	if err := RawSnpReport(testdata.AttestationBytes, &Options{
		Getter: getter,
		Product: &pb.SevProduct{
			Name:            pb.SevProduct_SEV_PRODUCT_MILAN,
			MachineStepping: &wrapperspb.UInt32Value{Value: 0},
		}}); err != nil {
		t.Error(err)
	}
}

func TestKDSCertBackdated(t *testing.T) {
	if !test.TestUseKDS() {
		t.Skip()
	}
	getter := test.GetKDS(t)
	// Throttle requests to KDS.
	time.Sleep(10 * time.Second)
	bytes, err := getter.Get(fmt.Sprintf("https://kdsintf.amd.com/vcek/v1/%s/3ac3fe21e13fb0990eb28a802e3fb6a29483a6b0753590c951bdd3b8e53786184ca39e359669a2b76a1936776b564ea464cdce40c05f63c9b610c5068b006b5d?blSPL=2&teeSPL=0&snpSPL=5&ucodeSPL=68", product()))
	if err != nil {
		t.Skipf("Live KDS query failed: %v", err)
	}
	cert, err := x509.ParseCertificate(bytes)
	if err != nil {
		t.Fatalf("Could not parse live VCEK certificate: %v", err)
	}
	now := time.Now()
	if !cert.NotBefore.Before(now.Add(-23 * time.Hour)) {
		t.Fatalf("KDS has not backdated its certificates. NotBefore: %s, now: %s",
			cert.NotBefore.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}
