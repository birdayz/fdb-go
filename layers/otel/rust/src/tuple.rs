//! FoundationDB tuple encodings used by OTEL_V1's primary and VALUE index keys.
#[derive(Clone, Debug)]
pub enum Element<'a> {
    Null,
    Bytes(&'a [u8]),
    String(&'a str),
    Int(i64),
    Double(f64),
    Bool(bool),
}

pub fn pack(elements: &[Element<'_>]) -> Vec<u8> {
    let mut out = Vec::new();
    for e in elements {
        match e {
            Element::Null => out.push(0),
            Element::Bytes(b) => escaped(&mut out, 1, b),
            Element::String(s) => escaped(&mut out, 2, s.as_bytes()),
            Element::Int(n) => {
                let magnitude = n.unsigned_abs();
                let size = (64 - magnitude.leading_zeros()).div_ceil(8) as usize;
                out.push(if *n < 0 {
                    0x14 - size as u8
                } else {
                    0x14 + size as u8
                });
                let bytes = magnitude.to_be_bytes();
                for b in &bytes[8 - size..] {
                    out.push(if *n < 0 { !b } else { *b });
                }
            }
            Element::Double(n) => {
                out.push(0x21);
                let bits = n.to_bits();
                let sortable = if bits >> 63 != 0 {
                    !bits
                } else {
                    bits ^ (1 << 63)
                };
                out.extend_from_slice(&sortable.to_be_bytes());
            }
            Element::Bool(b) => out.push(if *b { 0x27 } else { 0x26 }),
        }
    }
    out
}

fn escaped(out: &mut Vec<u8>, code: u8, bytes: &[u8]) {
    out.push(code);
    for b in bytes {
        out.push(*b);
        if *b == 0 {
            out.push(0xff);
        }
    }
    out.push(0);
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn literal_vectors_and_order() {
        for (n, expected) in [
            (0, "14"),
            (1, "1501"),
            (-1, "13fe"),
            (255, "15ff"),
            (256, "160100"),
            (-256, "12feff"),
            (i64::MIN, "0c7fffffffffffffff"),
        ] {
            assert_eq!(hex::encode(pack(&[Element::Int(n)])), expected);
        }
        assert_eq!(
            hex::encode(pack(&[Element::Bytes(&[0, 1]), Element::String("a\0b")])),
            "0100ff0100026100ff6200"
        );
        let values = [i64::MIN, -65536, -256, -1, 0, 1, 255, 256, i64::MAX];
        for pair in values.windows(2) {
            assert!(pack(&[Element::Int(pair[0])]) < pack(&[Element::Int(pair[1])]));
        }
    }
}
