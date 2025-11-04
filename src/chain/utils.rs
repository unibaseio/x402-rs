use alloy::{hex::FromHex, primitives::B256};

/// 解析签名 hex（支持 "0x" 前缀）或原始 bytes（64/65 字节）
/// 返回 (r: H256, s: H256, v_norm: u8) 其中 v_norm 已规范为 27 或 28
pub fn parse_signature_hex(sig_hex: &str) -> Result<(B256, B256, u8), String> {
    let s = sig_hex.strip_prefix("0x").unwrap_or(sig_hex);
    let bytes = Vec::from_hex(s).map_err(|e| format!("hex decode error: {}", e))?;
    parse_signature_bytes(&bytes)
}

pub fn parse_signature_bytes(bytes: &[u8]) -> Result<(B256, B256, u8), String> {
    match bytes.len() {
        65 => {
            let r = B256::from_slice(&bytes[0..32]);
            let s_val = B256::from_slice(&bytes[32..64]);
            let mut v = bytes[64];
            let v_norm = normalize_v(v);
            Ok((r, s_val, v_norm))
        }
        64 => {
            // EIP-2098 compact: r || vs, highest bit of vs encodes v parity
            let r = B256::from_slice(&bytes[0..32]);
            let mut vs = [0u8; 32];
            vs.copy_from_slice(&bytes[32..64]);
            let v_bit = (vs[0] & 0x80) != 0;
            vs[0] &= 0x7f; // clear highest bit to get s
            let s_val = B256::from_slice(&vs);
            let v_norm = if v_bit { 28u8 } else { 27u8 };
            Ok((r, s_val, v_norm))
        }
        other => Err(format!("unexpected signature length: {} bytes", other)),
    }
}

/// 规范化 v 到 27/28：
/// - 若 v == 0/1 -> +27
/// - 若 v >= 35 (EIP-155) -> 取 parity 并转成 27/28
/// - 否则若已是 27/28，直接返回
pub fn normalize_v(v: u8) -> u8 {
    if v == 0 || v == 1 {
        return v + 27;
    }
    if v == 27 || v == 28 {
        return v;
    }
    if v >= 35 {
        let parity = ((v as u64).wrapping_sub(35)) % 2;
        return (parity + 27) as u8;
    }
    // fallback: try to coerce but caller should handle unexpected values
    v
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_65_and_normalize() {
        let mut sig = vec![0u8; 65];
        for i in 0..32 {
            sig[i] = 0x11;
        }
        for i in 32..64 {
            sig[i] = 0x22;
        }
        sig[64] = 1; // v as 1
        let (r, s, v) = parse_signature_bytes(&sig).unwrap();
        assert_eq!(r.0[0], 0x11);
        assert_eq!(s.0[0], 0x22);
        assert_eq!(v, 28); // 1 -> +27 = 28
    }

    #[test]
    fn test_64_compact() {
        let mut r = vec![0x11u8; 32];
        let mut vs = vec![0x22u8; 32];
        vs[0] |= 0x80; // set highest bit: v = 28
        let mut sig = Vec::new();
        sig.extend_from_slice(&r);
        sig.extend_from_slice(&vs);
        let (r_out, s_out, v) = parse_signature_bytes(&sig).unwrap();
        assert_eq!(r_out.0[0], 0x11);
        assert_eq!(s_out.0[0], 0x22);
        assert_eq!(v, 28);
    }
}
