package utils

import (
    "crypto/rand"
    "crypto/rsa"
    "crypto/x509"
    "encoding/pem"
    "os"
)

// Générer une paire de clés RSA
func GenerateRSAKeys(user string) (*rsa.PrivateKey, error) {
    privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
    if err != nil {
        return nil, err
    }
    
    // Enregistrement des clés
    privateFile, err := os.Create(user + "-private.pem")
    if err != nil {
        return nil, err
    }
    pem.Encode(privateFile, &pem.Block{
        Type:  "RSA PRIVATE KEY",
        Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
    })
    privateFile.Close()

    publicKey := &privateKey.PublicKey
    publicFile, err := os.Create(user + "public.pem")
    if err != nil {
        return nil, err
    }
    pem.Encode(publicFile, &pem.Block{
        Type:  "RSA PUBLIC KEY",
        Bytes: x509.MarshalPKCS1PublicKey(publicKey),
    })
    publicFile.Close()

    return privateKey, nil
}
