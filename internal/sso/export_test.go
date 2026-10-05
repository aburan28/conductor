package sso

import "context"

// VerifyForTest verifies a raw ID token as Exchange would after redeeming a code.
func VerifyForTest(p Provider, raw, nonce string) (Identity, error) {
	o := p.(*oidcProvider)
	m, keys, err := o.discover(context.Background())
	if err != nil {
		return Identity{}, err
	}
	claims, err := o.verify(context.Background(), m, keys, raw, nonce)
	if err != nil {
		return Identity{}, err
	}
	return o.identity(claims)
}
