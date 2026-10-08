// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hosting

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strconv"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// AccessSecret reads a secret's latest enabled version: its value and its
// version number. The value is checked against Secret Manager's checksum when
// one comes with it.
func (c *Client) AccessSecret(ctx context.Context, project, secret string) (value []byte, version string, err error) {
	var out struct {
		Name    string `json:"name"`
		Payload struct {
			Data       string `json:"data"`
			DataCrc32c string `json:"dataCrc32c"`
		} `json:"payload"`
	}
	path := "/v1/projects/" + esc(project) + "/secrets/" + esc(secret) + "/versions/latest:access"
	if err := c.call(ctx, http.MethodGet, "secretmanager", path, nil, &out); err != nil {
		return nil, "", fmt.Errorf("reading secret %s: %w", secret, err)
	}
	data, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return nil, "", fmt.Errorf("reading secret %s: the value is not base64", secret)
	}
	if out.Payload.DataCrc32c != "" {
		want, err := strconv.ParseUint(out.Payload.DataCrc32c, 10, 32)
		if err != nil || uint32(want) != crc32.Checksum(data, castagnoli) {
			return nil, "", fmt.Errorf("reading secret %s: the value does not match its checksum", secret)
		}
	}
	version = lastSegment(out.Name)
	if version == "" {
		return nil, "", fmt.Errorf("reading secret %s: no version in the answer", secret)
	}
	return data, version, nil
}

// AddSecretVersion adds a version holding data to an existing secret and
// returns its version number.
func (c *Client) AddSecretVersion(ctx context.Context, project, secret string, data []byte) (version string, err error) {
	body, err := json.Marshal(map[string]any{"payload": map[string]string{
		"data":       base64.StdEncoding.EncodeToString(data),
		"dataCrc32c": strconv.FormatUint(uint64(crc32.Checksum(data, castagnoli)), 10),
	}})
	if err != nil {
		return "", err
	}
	var out struct {
		Name string `json:"name"`
	}
	path := "/v1/projects/" + esc(project) + "/secrets/" + esc(secret) + ":addVersion"
	if err := c.call(ctx, http.MethodPost, "secretmanager", path, bytes.NewReader(body), &out); err != nil {
		if IsNotFound(err) {
			return "", fmt.Errorf("secret %s does not exist in project %s; run `setup/gcp.sh secrets` first", secret, project)
		}
		return "", fmt.Errorf("adding a version to secret %s: %w", secret, err)
	}
	version = lastSegment(out.Name)
	if version == "" {
		return "", errors.New("adding a version to secret " + secret + ": no version in the answer")
	}
	return version, nil
}

// SecretExists reports whether a secret exists, versions or not. It needs
// secretmanager.secrets.get (Secret Manager Viewer), which setup grants the
// run account on leadscore-config-version only.
func (c *Client) SecretExists(ctx context.Context, project, secret string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "secretmanager", "/v1/projects/"+esc(project)+"/secrets/"+esc(secret), nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
