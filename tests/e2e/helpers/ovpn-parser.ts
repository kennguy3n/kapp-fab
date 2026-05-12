/**
 * Minimal .ovpn configuration file parser.
 *
 * Extracts commonly validated fields from an OpenVPN config string
 * so test assertions can verify server address, port, protocol, etc.
 */

export interface OvpnConfig {
  /** Raw file content. */
  raw: string;
  /** `remote` directive value, e.g. "vpn.example.com 1194". */
  remote: string | null;
  /** Server hostname extracted from the `remote` directive. */
  serverHost: string | null;
  /** Port extracted from the `remote` directive. */
  serverPort: string | null;
  /** Protocol (udp | tcp) from the `proto` directive. */
  proto: string | null;
  /** Device type from the `dev` directive (tun | tap). */
  dev: string | null;
  /** Cipher suite from the `cipher` directive. */
  cipher: string | null;
  /** Whether a `<ca>` inline certificate block is present. */
  hasCaCert: boolean;
  /** Whether a `<cert>` inline certificate block is present. */
  hasClientCert: boolean;
  /** Whether a `<key>` inline key block is present. */
  hasClientKey: boolean;
  /** Whether a `<tls-auth>` or `<tls-crypt>` block is present. */
  hasTlsAuth: boolean;
}

/**
 * Parse a raw .ovpn file string into structured fields.
 */
export function parseOvpnConfig(content: string): OvpnConfig {
  const directive = (name: string): string | null => {
    const regex = new RegExp(`^${name}\\s+(.+)$`, "m");
    const match = content.match(regex);
    return match ? match[1].trim() : null;
  };

  const remote = directive("remote");
  let serverHost: string | null = null;
  let serverPort: string | null = null;

  if (remote) {
    const parts = remote.split(/\s+/);
    serverHost = parts[0] ?? null;
    serverPort = parts[1] ?? null;
  }

  return {
    raw: content,
    remote,
    serverHost,
    serverPort,
    proto: directive("proto"),
    dev: directive("dev"),
    cipher: directive("cipher"),
    hasCaCert: /<ca>/.test(content),
    hasClientCert: /<cert>/.test(content),
    hasClientKey: /<key>/.test(content),
    hasTlsAuth: /<tls-auth>/.test(content) || /<tls-crypt>/.test(content),
  };
}
