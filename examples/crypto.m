// Security and CommonCrypto guest: digests, HMAC, AES, key derivation, random, and in-memory
// SecKey generation, signing, key agreement and certificate parsing. Keys are made the way iOS
// makes them (kSecUseDataProtectionKeychain, and not permanent, so no keychain is involved): the
// macOS default is the legacy CDSA stack, which needs securityd and its module database.
#import <Foundation/Foundation.h>
#import <CommonCrypto/CommonCrypto.h>
#import <CommonCrypto/CommonRandom.h>
#import <Security/Security.h>

static const char *hex(const void *bytes, size_t n) {
    NSMutableString *s = [NSMutableString string];
    for (size_t i = 0; i < n; i++) [s appendFormat:@"%02x", ((const uint8_t *)bytes)[i]];
    return s.UTF8String;
}

int main(void) {
    @autoreleasepool {
        const char *msg = "The quick brown fox jumps over the lazy dog";
        uint8_t md[CC_SHA512_DIGEST_LENGTH];
        CC_SHA1(msg, (CC_LONG)strlen(msg), md);   printf("sha1   %s\n", hex(md, CC_SHA1_DIGEST_LENGTH));
        CC_SHA256(msg, (CC_LONG)strlen(msg), md); printf("sha256 %s\n", hex(md, CC_SHA256_DIGEST_LENGTH));
        CC_SHA512(msg, (CC_LONG)strlen(msg), md); printf("sha512 %s\n", hex(md, 16));
        CCHmac(kCCHmacAlgSHA256, "key", 3, msg, strlen(msg), md); printf("hmac   %s\n", hex(md, CC_SHA256_DIGEST_LENGTH));

        uint8_t key[32], iv[16] = {0}, enc[64], dec[64];
        int rc = CCKeyDerivationPBKDF(kCCPBKDF2, "password", 8, (const uint8_t *)"salt", 4, kCCPRFHmacAlgSHA256, 4096, key, sizeof key);
        printf("pbkdf2 %d %s\n", rc, hex(key, sizeof key));
        size_t encLen = 0, decLen = 0;
        rc = CCCrypt(kCCEncrypt, kCCAlgorithmAES, kCCOptionPKCS7Padding, key, sizeof key, iv, msg, strlen(msg), enc, sizeof enc, &encLen);
        printf("aes    %d %zu %s\n", rc, encLen, hex(enc, 16));
        rc = CCCrypt(kCCDecrypt, kCCAlgorithmAES, kCCOptionPKCS7Padding, key, sizeof key, iv, enc, encLen, dec, sizeof dec, &decLen);
        printf("aes    %d round trip %d\n", rc, decLen == strlen(msg) && !memcmp(dec, msg, decLen));

        uint8_t rnd[16];
        printf("random %d %d\n", SecRandomCopyBytes(kSecRandomDefault, sizeof rnd, rnd), CCRandomGenerateBytes(rnd, sizeof rnd));

        CFErrorRef err = NULL;
        NSDictionary *ec = @{(id)kSecAttrKeyType: (id)kSecAttrKeyTypeECSECPrimeRandom, (id)kSecAttrKeySizeInBits: @256,
                             (id)kSecUseDataProtectionKeychain: @YES};
        SecKeyRef priv = SecKeyCreateRandomKey((CFDictionaryRef)ec, &err);
        if (!priv) { printf("ec key: %s\n", [(__bridge id)err description].UTF8String); return 1; }
        SecKeyRef pub = SecKeyCopyPublicKey(priv);
        NSData *data = [NSData dataWithBytes:msg length:strlen(msg)];
        NSData *sig = CFBridgingRelease(SecKeyCreateSignature(priv, kSecKeyAlgorithmECDSASignatureMessageX962SHA256, (CFDataRef)data, &err));
        printf("ecdsa  signature %d verify %d\n", sig.length > 60,
               SecKeyVerifySignature(pub, kSecKeyAlgorithmECDSASignatureMessageX962SHA256, (CFDataRef)data, (CFDataRef)sig, &err));
        NSData *pubData = CFBridgingRelease(SecKeyCopyExternalRepresentation(pub, &err));
        printf("ec     public key %lu bytes\n", (unsigned long)pubData.length);
        SecKeyRef peer = SecKeyCreateRandomKey((CFDictionaryRef)ec, &err);
        NSData *s1 = CFBridgingRelease(SecKeyCopyKeyExchangeResult(priv, kSecKeyAlgorithmECDHKeyExchangeStandard, SecKeyCopyPublicKey(peer), (CFDictionaryRef)@{}, &err));
        NSData *s2 = CFBridgingRelease(SecKeyCopyKeyExchangeResult(peer, kSecKeyAlgorithmECDHKeyExchangeStandard, pub, (CFDictionaryRef)@{}, &err));
        printf("ecdh   %lu bytes agree %d\n", (unsigned long)s1.length, [s1 isEqual:s2]);

        NSDictionary *rsa = @{(id)kSecAttrKeyType: (id)kSecAttrKeyTypeRSA, (id)kSecAttrKeySizeInBits: @2048,
                              (id)kSecUseDataProtectionKeychain: @YES};
        SecKeyRef rpriv = SecKeyCreateRandomKey((CFDictionaryRef)rsa, &err);
        if (!rpriv) { printf("rsa key: %s\n", [(__bridge id)err description].UTF8String); return 1; }
        SecKeyRef rpub = SecKeyCopyPublicKey(rpriv);
        NSData *ct = CFBridgingRelease(SecKeyCreateEncryptedData(rpub, kSecKeyAlgorithmRSAEncryptionOAEPSHA256, (CFDataRef)data, &err));
        NSData *pt = CFBridgingRelease(SecKeyCreateDecryptedData(rpriv, kSecKeyAlgorithmRSAEncryptionOAEPSHA256, (CFDataRef)ct, &err));
        printf("rsa    %lu bytes round trip %d\n", (unsigned long)ct.length, [pt isEqual:data]);

        // A self-signed P-256 certificate (CN=ipswrun test), parsed without trust evaluation.
        NSData *der = [[NSData alloc] initWithBase64EncodedString:
            @"MIIBYjCCAQigAwIBAgIUScELC0/r5jW3yAL3MIyhQUf5viswCgYIKoZIzj0EAwIwFzEVMBMGA1UEAwwMaXBzd3J1biB0ZXN0MB4X"
            @"DTI2MTExODE3MzE0M1oXDTM2MTExNTE3MzE0M1owFzEVMBMGA1UEAwwMaXBzd3J1biB0ZXN0MFkwEwYHKoZIzj0CAQYIKoZIzj0D"
            @"AQcDQgAEzuKA+G61ikkYt6UlkP3STUHgKQSqZkPG5UvmyA9k7jfdIAgaV3Gy2WtQEcal1En1VG2qLyn7bwvNuCNwWGfymKMyMDAw"
            @"HQYDVR0OBBYEFDbuERmPJtToQzHOWzgXu8KlNCguMA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDSAAwRQIgcabGeZDn416V"
            @"q5Q5hqQJ6kp2W5O/Iqwo0gcOfb+qoOgCIQDlABpXJkjhVBSkUUni+6t6IT7Leka8MyFEYKkBMEcV/w=="
            options:0];
        SecCertificateRef cert = SecCertificateCreateWithData(NULL, (CFDataRef)der);
        NSString *summary = cert ? CFBridgingRelease(SecCertificateCopySubjectSummary(cert)) : nil;
        SecKeyRef certKey = cert ? SecCertificateCopyKey(cert) : NULL;
        printf("cert   %s key %d\n", summary.UTF8String ?: "unparsed", certKey != NULL);
        if (err) printf("error %s\n", [(__bridge id)err description].UTF8String);
    }
    return 0;
}
