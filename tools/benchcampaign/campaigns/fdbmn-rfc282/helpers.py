"""Reuse existing pure validation/numerical helpers; never historical populations."""
from pathlib import Path
import sys

LEGACY = Path(__file__).resolve().parent.parent / 'fdbmn-20261020' / 'public-export'
sys.path.insert(0, str(LEGACY))
import export_public_results as legacy_io
import export_throughput as legacy_m1
import m2_throughput as legacy_m2
