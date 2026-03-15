# Quick Start: Implementing Speed Optimizations

## Step 1: Increase Read Chunk Size (5 minutes)

### Change the code:
```bash
cd c:\Users\Kian\SlipStream-Plus\slipstream-rust
```

Edit `crates/slipstream-client/src/streams/io_tasks.rs`:
- Line ~18: Change `pub(super) const STREAM_READ_CHUNK_BYTES: usize = 4096;`
- To: `pub(super) const STREAM_READ_CHUNK_BYTES: usize = 65536;` (64KB)

### Rebuild:
```bash
cargo build -p slipstream-client --release
```

---

## Step 2: Configure Flow Control (1 minute)

When starting the client, set:

```powershell
# PowerShell
$env:SLIPSTREAM_STREAM_QUEUE_MAX_BYTES = "16777216"   # 16MB
$env:SLIPSTREAM_CONN_RESERVE_BYTES = "2097152"        # 2MB
$env:RUST_LOG = "info"

# Then run:
.\target\release\slipstream-client.exe --resolver <IP:PORT> --domain <domain> -l 5201
```

---

## Step 3: Test with Benchmarks

If you have Linux tools available in WSL:

```bash
cd slipstream-rust
TRANSFER_BYTES=10485760 ./scripts/bench/run_rust_rust_10mb.sh
```

Or measure manually with iperf3 against the proxy.

---

## Step 4 (Optional): Aggressive Pacing

Edit `crates/slipstream-client/src/pacing.rs`:
- Line ~6-7: Change `const PACING_GAIN_PROBE: f64 = 1.25;`
- To: `const PACING_GAIN_PROBE: f64 = 1.50;` (or 1.75 for very aggressive)

Rebuild and test.

---

## Step 5: Enable GSO

Add flag to client startup:

```powershell
.\target\release\slipstream-client.exe `
  --resolver <IP:PORT> `
  --domain <domain> `
  -l 5201 `
  -g true
```

---

## Measurement Tips

After each change, measure:

```bash
# Upload speed (exfil)
curl -T largefile.bin http://localhost:5201

# Download speed
curl http://localhost:5201/largefile.bin -o /tmp/out.bin

# Use iperf3 for more accurate measurement:
iperf3 -c localhost -p 5201 -t 30 -P 4  # 4 parallel streams
```

---

## 🎯 Expected Results

| Configuration | Expected MiB/s |
|---------------|-----------------|
| Default | ~10 MiB/s |
| After Step 1-2 | ~15-18 MiB/s |
| After Step 1-4 | ~18-22 MiB/s |
| After Step 1-5 | ~20-30 MiB/s (if GSO works) |

Results vary by network conditions and resolver speed.

