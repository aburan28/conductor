import CryptoKit
import Foundation

guard CommandLine.arguments.count == 2,
      let encoded = try? String(contentsOfFile: CommandLine.arguments[1], encoding: .utf8),
      let key = Data(base64Encoded: encoded.trimmingCharacters(in: .whitespacesAndNewlines)),
      key.count == 32 || key.count == 96 else {
    fputs("expected a Sparkle private key file containing a base64 32-byte seed or legacy 96-byte key\n", stderr)
    exit(1)
}

do {
    if key.count == 96 {
        // Sparkle's older format concatenates a 64-byte private key and 32-byte public key.
        print(Data(key.suffix(32)).base64EncodedString())
    } else {
        let privateKey = try Curve25519.Signing.PrivateKey(rawRepresentation: key)
        print(privateKey.publicKey.rawRepresentation.base64EncodedString())
    }
} catch {
    fputs("could not derive the Sparkle public key\n", stderr)
    exit(1)
}
