// Copyright 2019 The Berglas Authors
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

package berglas

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"regexp"
	"strings"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/storage"
	"github.com/pkg/errors"
	"google.golang.org/api/option"
)

const (
	Name       = "berglas"
	Version    = "0.1.0"
	ProjectURL = "https://github.com/GoogleCloudPlatform/berglas"
	UserAgent  = Name + "/" + Version + " (+" + ProjectURL + ")"
)

const (
	// CacheControl is the cache-control value to set on the GCS objects. This is
	// configured to use no caching, since users most likely want their secrets to
	// be immediately available.
	CacheControl = "private, no-cache, no-store, no-transform, max-age=0"

	// MeatadataIDKey is a key in the object metadata that identifies an
	// object as a secret. This is used when enumerating secrets in a bucket, in
	// case non-secrets also reside in the bucket.
	MetadataIDKey = "berglas-secret"

	// MetadataKMSKey is the key in the metadata where the name of the KMS key is
	// stored.
	MetadataKMSKey = "berglas-kms-key"
)

// Client is a berglas client
type Client struct {
	kmsClient     *kms.KeyManagementClient
	storageClient *storage.Client
}

// New creates a new berglas client.
func New(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	opts = append(opts, option.WithUserAgent(UserAgent))

	var c Client

	kmsClient, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create kms client")
	}
	c.kmsClient = kmsClient

	storageClient, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create storage client")
	}
	c.storageClient = storageClient

	return &c, nil
}

// KMSKeyIncludesVersion returns true if the given KMS key reference includes
// a version.
func KMSKeyIncludesVersion(s string) bool {
	return strings.Count(s, "/") > 7
}

// KMSKeyTrimVersion trims the version from a KMS key reference if it exists.
func KMSKeyTrimVersion(s string) string {
	if !KMSKeyIncludesVersion(s) {
		return s
	}

	parts := strings.SplitN(s, "/", 9)
	return strings.Join(parts[0:8], "/")
}

type kmsRef struct {
	project, location, keyRing, cryptoKey, version string
}

var (
	kmsRe = regexp.MustCompile(`projects/(?P<project>[^/]+)/locations/(?P<location>[^/]+)/keyRings/(?P<keyRing>[^/]+)/cryptoKeys/(?P<cryptoKey>[^/]+)(/cryptoKeyVersions/(?P<version>[^/]+))?`)
)

func parseKMSRef(s string) (*kmsRef, error) {
	parts := kmsRe.FindStringSubmatch(s)
	if len(parts) < 5 {
		return nil, errors.Errorf("malformed KMS key: %s", s)
	}

	var k kmsRef
	k.project = parts[1]
	k.location = parts[2]
	k.keyRing = parts[3]
	k.cryptoKey = parts[4]

	if len(parts) > 5 {
		k.version = parts[5]
	}

	return &k, nil
}

// envelopeDecrypt decrypts the data with the dek, returning the plaintext and
// any errors that occur.
func envelopeDecrypt(dek, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cipher from dek")
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create gcm from dek")
	}

	size := aesgcm.NonceSize()
	if len(data) < size {
		return nil, errors.New("malformed ciphertext")
	}
	nonce, ciphertext := data[:size], data[size:]

	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decrypt ciphertext with dek")
	}
	return plaintext, nil
}

// envelopeEncrypt generates a unique DEK and encrypts the plaintext with the
// given key. The encryption key and resulting ciphertext are returned.
func envelopeEncrypt(plaintext []byte) ([]byte, []byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate random key bytes")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create cipher from key")
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create gcm cipher")
	}

	// Generate nonce
	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate random nonce bytes")
	}

	// Encrypt the ciphertext with the DEK
	ciphertext := aesgcm.Seal(nonce, nonce, plaintext, nil)

	return key, ciphertext, nil
}