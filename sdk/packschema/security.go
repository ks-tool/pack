package packschema

// CertJava TLS-serving role mount for Java application
type CertJava struct {
	//Keytab     packschema.Secret `json:"keytab" jsonschema:"required,title=Keytab роли" pack:"file=/etc/security/keytabs/service.keytab,format=secret,mode=0400"`
	Keystore   Secret `json:"keystore,omitzero" jsonschema:"title=Keystore (JKS) с сертификатом хоста" pack:"file=/etc/security/tls/keystore.jks,format=secret,mode=0440"`
	Truststore Secret `json:"truststore" jsonschema:"required,title=Truststore (JKS) с CA стенда" pack:"file=/etc/security/tls/truststore.jks,format=secret,mode=0444"`
	CACert     Secret `json:"caCert" jsonschema:"title=CA стенда (PEM)" pack:"file=/etc/security/tls/ca.crt,format=secret,mode=0444"`
}

// CertPEM is the server certificate and key for a role
type CertPEM struct {
	Cert   Secret `json:"cert" jsonschema:"required,title=Сертификат хоста (PEM)" pack:"file=/etc/security/tls/server.crt,format=secret,mode=0444"`
	Key    Secret `json:"key" jsonschema:"required,title=Ключ хоста (PEM)" pack:"file=/etc/security/tls/server.key,format=secret,mode=0440"`
	CACert Secret `json:"caCert" jsonschema:"title=CA стенда (PEM)" pack:"file=/etc/security/tls/ca.crt,format=secret,mode=0444"`
}

// Vault is the openbao credential provider the Hadoop stack reads passwords
// from: the address comes from the neighbor, the token is a Secret in the
// environment (the provider reads VAULT_TOKEN; it never lands in a file).
type Vault struct {
	Token Secret `json:"token" jsonschema:"required,title=Токен openbao" pack:"env=VAULT_TOKEN"`
}
