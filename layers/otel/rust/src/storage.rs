//! Java-readable transformed records and the split-record boundary. Compression
//! implementations may produce different deflate bytes; decoded unions must match.
use crate::{Error, ErrorKind, Result};
use flate2::{Compression, read::ZlibDecoder, write::ZlibEncoder};
use std::{
    cell::RefCell,
    io::{Read, Write},
};

thread_local! {
    /// One reusable zlib encoder per worker thread and level (1 and 6). It is reset by
    /// every record's `finish`, so no state crosses records. Created on first use.
    static ENCODERS: RefCell<[Option<ZlibEncoder<Vec<u8>>>; 2]> = const { RefCell::new([None, None]) };
}

/// Zlib-format deflate of `input` at `level` (1 or 6) with this thread's reused encoder.
fn deflate(input: &[u8], level: u32) -> Result<Vec<u8>> {
    let fail = |_| Error::new(ErrorKind::Operation, "zlib encode");
    ENCODERS.with(|slots| {
        let mut slots = slots.borrow_mut();
        let encoder = slots[usize::from(level != 1)]
            .get_or_insert_with(|| ZlibEncoder::new(Vec::new(), Compression::new(level)));
        let result = encoder.write_all(input);
        // `reset` completes the stream, returns it and readies the encoder for the next
        // record, even after a failed write.
        let finished = encoder.reset(Vec::with_capacity(input.len() / 2 + 64));
        result.map_err(fail)?;
        finished.map_err(fail)
    })
}

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
    // Go attempts compression only above 5 bytes and keeps it only if it saves more than 5.
    if union.len() <= 5 {
        let mut out = vec![2];
        out.extend_from_slice(&union);
        return Ok(out);
    }
    let deflate = deflate(
        &union,
        if matches!(compression, RecordCompression::Zlib1) {
            1
        } else {
            6
        },
    )?;
    if deflate.len() < union.len() - 5 {
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
    use flate2::write::ZlibEncoder;
    use std::io::Write;

    /// A new compressor per record, as before the reuse change.
    fn fresh(union: &[u8], level: u32) -> Vec<u8> {
        let mut encoder = ZlibEncoder::new(Vec::new(), Compression::new(level));
        encoder.write_all(union).unwrap();
        encoder.finish().unwrap()
    }
    fn body(seed: u64, len: usize) -> Vec<u8> {
        let mut x = seed | 1;
        (0..len)
            .map(|i| {
                x ^= x << 13;
                x ^= x >> 7;
                x ^= x << 17;
                // Half compressible text, half noise.
                if i % 2 == 0 {
                    b'a' + (x % 5) as u8
                } else {
                    (x >> 24) as u8
                }
            })
            .collect()
    }

    #[test]
    fn reused_compressor_matches_a_fresh_one_and_leaks_nothing_between_records() {
        // Large, tiny, empty-ish and incompressible records in alternating order and levels,
        // all on this one thread's reused compressors.
        let sizes = [
            200_000, 7, 6, 1, 50_000, 0, 130_000, 7, 100_000, 33, 200_001,
        ];
        for round in 0..3u64 {
            for (i, &n) in sizes.iter().enumerate() {
                for level in [1, 6] {
                    let input = body(round * 100 + i as u64, n);
                    assert_eq!(deflate(&input, level).unwrap(), fresh(&input, level));
                }
            }
        }
        // End to end through `encode`: the stored bytes decode to the row, in any order.
        for &n in &sizes {
            let row = body(n as u64, n);
            for mode in [RecordCompression::Zlib1, RecordCompression::ZlibDefault] {
                let stored = encode(&row, mode).unwrap();
                assert_eq!(decode(&stored).unwrap(), row);
            }
        }
    }

    #[test]
    fn compressors_are_created_once_per_thread_and_level_and_kept_only_if_smaller() {
        let slots = || ENCODERS.with(|s| s.borrow().iter().filter(|c| c.is_some()).count());
        std::thread::spawn(move || {
            assert_eq!(slots(), 0);
            // Under 6 bytes the union is stored clear without compressing (nothing created).
            assert_eq!(encode(&[], RecordCompression::Zlib1).unwrap(), [2, 0x0a, 0]);
            assert_eq!(slots(), 0);
            let row = vec![7; 1000];
            for _ in 0..3 {
                assert_eq!(encode(&row, RecordCompression::Zlib1).unwrap()[..2], [4, 1]);
            }
            assert_eq!(slots(), 1);
            encode(&row, RecordCompression::ZlibDefault).unwrap();
            assert_eq!(slots(), 2);
            // Incompressible input is stored clear (type 2) even though a compressor ran.
            let noise = body(9, 64)
                .into_iter()
                .map(|b| b | 0x80)
                .collect::<Vec<_>>();
            let stored = encode(&noise, RecordCompression::Zlib1).unwrap();
            assert_eq!(stored[0], 2);
            assert_eq!(decode(&stored).unwrap(), noise);
        })
        .join()
        .unwrap();
    }
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
