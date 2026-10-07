// SPDX-License-Identifier: MIT

package semantic

// Where the model is published and what it must hash to. The hash is what the
// download is held to: a file that arrives different is thrown away, whatever
// the server that sent it says.
const (
	ModelURL          = "https://github.com/BlankTrail/google-serp-parser/releases/download/semantic-model-v1/" + FileName
	ModelSHA256       = "9b35cf0d631fa40248bb35e11c00bcdefa3de6f0e2c39940576270a0356b7cb0"
	ModelSize   int64 = 139186847
)
