// Wires the AES-GCM helpers into db/payment_providers.go.
//
// db cannot import lib (lib already imports db for token invalidation and
// operation verification), so the db layer exposes a registration seam. Keeping
// the codec here means payment-provider configs use exactly the same key
// derivation as the rest of the sensitive-data storage.
package lib

import "github.com/rain-kl/payincus/server-go/src/db"

func init() {
	db.RegisterPaymentProviderSensitiveCodec(EncryptSensitiveData, DecryptSensitiveData)
}
