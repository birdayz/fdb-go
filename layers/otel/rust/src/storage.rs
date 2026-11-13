//! Java-readable transformed records and the split-record boundary. Compression
//! implementations may produce different deflate bytes; decoded unions must match.
use crate::{Error, ErrorKind, Result};
use flate2::{Compression, read::ZlibDecoder, write::ZlibEncoder};
use std::io::{Read, Write};

#[derive(Clone, Copy, Debug, serde::Deserialize, serde::Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum RecordCompression {
    None,
    Zlib1,
    ZlibDefault,
}

pub fn encode(row: &[u8], compression: RecordCompression) -> Result<Vec<u8>> {
    // RecordTypeUnion.SPANS_0 is field 1 in the pinned OTEL_V1 descriptor.
    let mut union = vec![0x0a];
    varint(&mut union, row.len() as u64);
    union.extend_from_slice(row);
    if matches!(compression, RecordCompression::None) {
        return Ok(union);
    }
    let mut encoder = ZlibEncoder::new(
        Vec::new(),
        Compression::new(if matches!(compression, RecordCompression::Zlib1) {
            1
        } else {
            6
        }),
    );
    encoder
        .write_all(&union)
        .map_err(|_| Error::new(ErrorKind::Operation, "zlib encode"))?;
    let deflate = encoder
        .finish()
        .map_err(|_| Error::new(ErrorKind::Operation, "zlib finish"))?;
    if union.len() > 5 && deflate.len() < union.len() - 5 {
        let mut out = vec![4, 1];
        out.extend_from_slice(&(union.len() as u32).to_be_bytes());
        out.extend_from_slice(&deflate);
        Ok(out)
    } else {
        let mut out = vec![2];
        out.extend_from_slice(&union);
        Ok(out)
    }
}

pub fn decode(stored: &[u8]) -> Result<Vec<u8>> {
    let corrupt = || Error::new(ErrorKind::Corruption, "invalid SPANS transformed record");
    let union = match stored.first() {
        Some(0x0a) => stored.to_vec(),
        Some(2) => stored[1..].to_vec(),
        Some(4) if stored.len() >= 6 && stored[1] == 1 => {
            let size = u32::from_be_bytes(stored[2..6].try_into().unwrap()) as usize;
            if size > 16 * 1024 * 1024 {
                return Err(corrupt());
            }
            let mut out = Vec::new();
            let mut decoder = ZlibDecoder::new(&stored[6..]);
            decoder
                .by_ref()
                .take(size as u64 + 1)
                .read_to_end(&mut out)
                .map_err(|_| corrupt())?;
            if out.len() != size || decoder.total_in() as usize != stored.len() - 6 {
                return Err(corrupt());
            }
            out
        }
        _ => return Err(corrupt()),
    };
    if union.first() != Some(&0x0a) {
        return Err(corrupt());
    }
    let mut len = 0usize;
    for (i, b) in union[1..].iter().enumerate().take(5) {
        len |= ((*b & 0x7f) as usize) << (7 * i);
        if b & 0x80 == 0 {
            if len != union.len() - i - 2 {
                return Err(corrupt());
            }
            return Ok(union[i + 2..].to_vec());
        }
    }
    Err(corrupt())
}

fn varint(out: &mut Vec<u8>, mut n: u64) {
    while n >= 128 {
        out.push(n as u8 | 0x80);
        n >>= 7;
    }
    out.push(n as u8);
}

pub fn split(stored: &[u8]) -> Vec<(i64, Vec<u8>)> {
    if stored.len() <= 100_000 {
        return vec![(0, stored.to_vec())];
    }
    stored
        .chunks(100_000)
        .enumerate()
        .map(|(i, bytes)| (i as i64 + 1, bytes.to_vec()))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn roundtrip_and_boundaries() {
        for size in [0, 1, 127, 128, 99_996, 100_000, 200_001] {
            let row = vec![42; size];
            for mode in [
                RecordCompression::None,
                RecordCompression::Zlib1,
                RecordCompression::ZlibDefault,
            ] {
                let stored = encode(&row, mode).unwrap();
                assert_eq!(decode(&stored).unwrap(), row);
                let chunks = split(&stored);
                assert!(chunks.iter().all(|(_, c)| c.len() <= 100_000));
                assert_eq!(
                    chunks
                        .iter()
                        .flat_map(|(_, c)| c.clone())
                        .collect::<Vec<_>>(),
                    stored
                );
                assert_eq!(chunks[0].0, if stored.len() <= 100_000 { 0 } else { 1 });
            }
        }
    }
    #[test]
    fn reject_malformed_encrypted_unknown_and_bombs() {
        for data in [
            vec![],
            vec![1],
            vec![4, 2, 0, 0, 0, 0],
            vec![4, 1, 255, 255, 255, 255],
            vec![10, 2, 0],
            vec![10, 0, 0],
            vec![2, 18, 0],
        ] {
            assert_eq!(decode(&data).unwrap_err().kind, ErrorKind::Corruption);
        }
    }
}
