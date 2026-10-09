import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.Signature;
import java.security.cert.CertificateFactory;

// 离线包检查复用 JDK 加密实现；设备端仍以宿主发行证书决定是否信任。
final class PluginSignature {
    public static void main(String[] args) throws Exception {
        byte[] certificate = Files.readAllBytes(Path.of(args[2]));
        var cert = CertificateFactory.getInstance("X.509").generateCertificate(new java.io.ByteArrayInputStream(certificate));
        Signature verifier = Signature.getInstance("SHA256withRSA");
        verifier.initVerify(cert.getPublicKey()); verifier.update(Files.readAllBytes(Path.of(args[0])));
        if (!verifier.verify(Files.readAllBytes(Path.of(args[1])))) throw new SecurityException("Invalid plugin signature");
        System.out.println(java.util.HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(certificate)));
    }
}
