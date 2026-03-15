# SlipStream-Rust Client Bandwidth و Speed Optimization Guide

## خلاصه تحلیل (Summary)

پس از تجزیه و تحلیل کامل کد منبع SlipStream-Rust client، تعدادی نقطه بهینه‌سازی برای افزایش سرعت انتقال داده شناسایی شده است. بیشتر این بهینه‌سازی‌ها مربوط به اندازه buffer، pacing، و flow control است.

---

## 1. نقاط Bottleneck موجود

### 1.1 اندازه Read Chunk کوچک
**فایل**: `crates/slipstream-client/src/streams/io_tasks.rs`
```rust
pub(super) const STREAM_READ_CHUNK_BYTES: usize = 4096;  // 4KB!
```

**مشکل**: 
- هر بار فقط 4KB داده خوانده می‌شود
- برای transfer بزرگ، این باعث context switches و syscalls بیشتری می‌شود
- Overhead برای هر 4KB نسبتاً بالا است

**راه حل**: `65536` (64KB) یا بیشتر

---

### 1.2 Flow Control Limits
**فایل**: `crates/slipstream-core/src/flow_control.rs`
```rust
const DEFAULT_STREAM_QUEUE_MAX_BYTES: usize = 2 * 1024 * 1024;     // 2MB
const DEFAULT_CONN_RESERVE_BYTES: usize = 64 * 1024;               // 64KB
```

**مشکل**:
- 2MB stream queue درخیلی کم است برای fast networks
- 64KB reserve بیشتر از حد محافظه‌کار است

**راه حل**: Environment variables موجود هستند:
```bash
export SLIPSTREAM_STREAM_QUEUE_MAX_BYTES=16777216          # 16MB
export SLIPSTREAM_CONN_RESERVE_BYTES=1048576               # 1MB
```

---

### 1.3 DNS Poll Timeout و Slice
**فایل**: `crates/slipstream-client/src/dns/poll.rs`
```rust
const AUTHORITATIVE_POLL_TIMEOUT_US: u64 = 5_000_000;  // 5 seconds!
```

**مشکل**: 
- Poll timeout بسیار بالا است (5 ثانیه)
- برای slow DNS responses، این timeout may cause delays

**راه حل**: Can be tuned or made configurable

---

### 1.4 Coalescing Max Bytes
**فایل**: `crates/slipstream-client/src/streams/io_tasks.rs`
```rust
pub(super) fn spawn_client_writer(
    ...
    coalesce_max_bytes: usize,  // How much data to batch?
) {
    while buffer.len() < coalesce_max_bytes {
        // Try to grab more data
    }
}
```

**مشکل**: 
- Coalescing limit نامشخص است (نیاز به تحقیق)
- اگر کم باشد، small writes = slow throughput

---

### 1.5 Pacing Gain Tuning
**فایل**: `crates/slipstream-client/src/pacing.rs`
```rust
const PACING_GAIN_BASE: f64 = 1.0;
const PACING_GAIN_PROBE: f64 = 1.25;
```

**مشکل**: 
- Probe gain 1.25x است (محافظه‌کار)
- برای high-bandwidth links، می‌توان آن را افزایش داد (e.g., 1.5x)

---

## 2. بهینه‌سازی‌های توصیه‌شده

### 2.1 تغییرات Immediate (فی الحال)

#### الف) افزایش Stream Read Chunk Size

**فایل**: `crates/slipstream-client/src/streams/io_tasks.rs`

```rust
// OLD:
pub(super) const STREAM_READ_CHUNK_BYTES: usize = 4096;

// NEW:
pub(super) const STREAM_READ_CHUNK_BYTES: usize = 65536;  // 64KB
```

**تاثیر**: 
- Expected throughput improvement: **10-20%** (کاهش context switches)
- Memory impact: Negligible (یک buffer از 65KB)

**Test command**:
```bash
cargo build -p slipstream-client --release
```

---

#### ب) Environment Variables برای Flow Control

تنظیم کنید **قبل** از اجرای client:

```bash
# For fast networks (1 Gbps+)
export SLIPSTREAM_STREAM_QUEUE_MAX_BYTES=16777216         # 16MB
export SLIPSTREAM_CONN_RESERVE_BYTES=2097152              # 2MB
export RUST_LOG=info

# سپس شروع client
slipstream-client --resolver IP:PORT --domain example.com -l 5201
```

**تاثیر**: 
- Expected improvement: **15-30%** (more buffering for RTT tolerance)
- Memory usage: +~20MB per connection

---

### 2.2 تغییرات Code-Level

#### الف) Increase Pacing Probe Gain

**فایل**: `crates/slipstream-client/src/pacing.rs`

```rust
// OLD:
const PACING_GAIN_BASE: f64 = 1.0;
const PACING_GAIN_PROBE: f64 = 1.25;

// NEW (Aggressive):
const PACING_GAIN_BASE: f64 = 1.0;
const PACING_GAIN_PROBE: f64 = 1.50;  // More aggressive probing

// OR (Very Aggressive):
const PACING_GAIN_BASE: f64 = 1.1;    // Baseline already higher
const PACING_GAIN_PROBE: f64 = 1.75;
```

**تاثیر**: 
- Expected improvement: **5-15%** (faster bandwidth probing)
- Risk: Potential packet loss on congested links
- **Recommendation**: Start with 1.50, monitor for packet loss

---

#### ب) Dynamic Coalescing Strategy

**فایل**: `crates/slipstream-client/src/streams/io_tasks.rs`

Current implementation tries to coalesce up to `coalesce_max_bytes`. 

**بهینه‌سازی**: Increase the coalescing threshold:

```rust
// Where spawn_client_writer is called, check what coalesce_max_bytes is set to
// Likely location: runtime.rs or acceptor.rs

// If currently using a fixed value, increase it:
// From (e.g.) 16KB to 65KB or 131KB
```

---

### 2.3 DNS-Level Optimizations

#### الف) Parallel Resolver Paths

Already supported! Use:

```bash
slipstream-client \
  --resolver NS1:53 \
  --resolver NS2:53 \
  --resolver NS3:53 \
  --domain example.com
```

**فائدہ**: 
- Multiple resolvers = better failover + diversity
- Expected: **5-10% improvement** if resolvers have different RTTs

---

#### ب) GSO (Generic Segmentation Offload)

Client supports this natively:

```bash
slipstream-client --resolver IP:PORT --domain example.com -g true
```

**تاثیر**: 
- System-level packet optimization (if your NIC/kernel support it)
- Expected: **10-20% improvement** (if available)

**Check if available**:
```bash
# On Linux
cat /proc/net/udp | grep udp_gso_segment
ethtool -k <interface> | grep generic-segmentation
```

---

## 3. Advanced Tuning

### 3.1 Congestion Control Algorithm

موجود است در runtime:

```bash
slipstream-client \
  --resolver IP:PORT \
  --domain example.com \
  --congestion-control bbr  # or 'dcubic' (default: auto-select)
```

**Comparison**:
- `bbr`: Better for lossy networks, faster convergence
- `dcubic`: More stable, traditional behavior
- Auto: Selects based on network conditions

**Recommendation**: Try both and measure!

---

### 3.2 Keep-Alive Tuning

```bash
slipstream-client \
  --resolver IP:PORT \
  --domain example.com \
  --keep-alive-interval 200  # milliseconds (default: 400)
```

**تاثیر**: 
- Lower interval = faster ack cycles (but more packets)
- Higher interval = fewer packets (but slower feedback)
- Default 400ms is reasonable; try 200-300ms for high-speed links

---

### 3.3 TCP Listen Backlog

در code پوشیده شده است - might need patch

---

## 4. Server-Side Optimizations

اگر شما server را هم کنترل می‌کنید:

```bash
# Increase max connections for better pacing balance
slipstream-server \
  --max-connections 512 \
  --idle-timeout-seconds 120 \
  --cert server.crt \
  --key server.key
```

---

## 5. Benchmarking و Measurement

### موجود در repo:

```bash
# 10MB transfer test
TRANSFER_BYTES=10485760 ./scripts/bench/run_rust_rust_10mb.sh

# With minimum bandwidth threshold
TRANSFER_BYTES=10485760 MIN_AVG_MIB_S=20 ./scripts/bench/run_rust_rust_10mb.sh

# With delay injection (simulate RTT)
NETEM_DELAY_MS=50 TRANSFER_BYTES=10485760 ./scripts/bench/run_rust_rust_10mb.sh
```

### Current Baseline (from docs/benchmarks-results.md):

```
Rust <-> Rust (loopback):
- Upload (Exfil): ~10.42 MiB/s
- Download: ~10.64 MiB/s

With delay (11.3ms + 2.7ms jitter):
- Upload: ~1.58 MiB/s (!)
- Download: ~4.96 MiB/s
```

**Observation**: Delay has MASSIVE impact. This suggests flow control limits are too conservative.

---

## 6. خطة اجرای عملی

### Phase 1: Quick Wins (20 دقیقه)

```bash
# 1. Rebuild with 64KB chunk size
# Edit: crates/slipstream-client/src/streams/io_tasks.rs
# Change: STREAM_READ_CHUNK_BYTES from 4096 to 65536

cargo build -p slipstream-client --release

# 2. Set environment variables
export SLIPSTREAM_STREAM_QUEUE_MAX_BYTES=16777216
export SLIPSTREAM_CONN_RESERVE_BYTES=2097152

# 3. Test
TRANSFER_BYTES=10485760 ./scripts/bench/run_rust_rust_10mb.sh > baseline_phase1.txt

# Measure improvement
```

### Phase 2: Pacing Tuning (30 دقیقه)

```bash
# 1. Try different PACING_GAIN_PROBE values (1.35, 1.50, 1.75)
# Edit: crates/slipstream-client/src/pacing.rs

# 2. Rebuild and test each variant
cargo build -p slipstream-client --release
TRANSFER_BYTES=10485760 ./scripts/bench/run_rust_rust_10mb.sh > baseline_phase2_<gain>.txt

# 3. Compare results
```

### Phase 3: Congestion Control comparison (20 دقیقه)

```bash
# Test BBR vs DCUBIC
./scripts/bench/run_rust_rust_10mb.sh --client-args="--congestion-control bbr" > bbr.txt
./scripts/bench/run_rust_rust_10mb.sh --client-args="--congestion-control dcubic" > dcubic.txt
```

### Phase 4: Server-Side Tuning (15 دقیقه)

```bash
# Increase server connections
# Edit server startup parameters
# Re-run benchmarks with modified server config
```

---

## 7. Expected Gains

Based on code analysis:

| Optimization | Expected Gain | Effort |
|--------------|--------------|--------|
| 64KB chunk size | +10-20% | 1 line |
| Flow control env vars | +15-30% | 2 commands |
| Pacing gain 1.50x | +5-15% | 1 line |
| GSO enabled | +10-20% | 1 flag |
| Congestion control tuning | +5-10% | Testing |
| Multiple resolvers | +5-10% | Config |
| **Combined conservative** | **+40-60%** | Low |
| **Combined aggressive** | **+60-100%** | Medium |

---

## 8. Cautions

⚠️ **عوارض جانبی ممکن**:

1. **Higher coalescing** → Higher latency for small transfers
2. **Probe gain > 1.5** → Potential packet loss on congested networks
3. **Large flow control buffers** → Higher memory usage
4. **Multiple resolvers** → May hit resolver rate limits

---

## 9. منابع و مراجع

1. **Configuration**: `docs/config.md`
2. **Profiling**: `docs/profiling.md`
3. **Benchmarks**: `docs/benchmarks.md`
4. **Source Code**:
   - Flow control: `crates/slipstream-core/src/flow_control.rs`
   - IO tasks: `crates/slipstream-client/src/streams/io_tasks.rs`
   - Pacing: `crates/slipstream-client/src/pacing.rs`
   - DNS polling: `crates/slipstream-client/src/dns/poll.rs`

---

## خلاصه نهایی

بهترین استراتژی برای شروع:

1. ✅ Increase `STREAM_READ_CHUNK_BYTES` to 64KB
2. ✅ Set flow control env vars (16MB stream, 2MB reserve)
3. ✅ Test with `--congestion-control bbr`
4. ✅ Use multiple resolvers if available
5. ✅ Enable GSO if supported
6. ⚠️ Tune `PACING_GAIN_PROBE` only after baseline measurements

ای امید way to quickly validate improvements: Run benchmark suite before and after each change.

