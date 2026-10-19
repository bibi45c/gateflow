# Real-model cloud session

The runbook for tests T0–T5 against a real model on EKS (issue
[#146](https://github.com/bibi45c/gateflow/issues/146)). It uses:

- the GPU model host of [#144](https://github.com/bibi45c/gateflow/issues/144): vLLM
  v0.31.0 serving Qwen3-0.6B in FP16 on one T4;
- the prompts and checks of [#145](https://github.com/bibi45c/gateflow/issues/145):
  `loadtest/prompts/realmodel.jsonl`, `loadtest/realmodel_eval.py`, and the
  `--prompts` and `--gateway-url` options of `loadtest/kube_run.py`.

It extends a normal cloud day ([Flow example](README.md#flow-example)); everything not
said here is as there. **bibi45c only**, or a session the owner directs. Every step is
paid, so it needs the owner's approval for that session.

| Test | Question | Path |
|---|---|---|
| T0 | What can vLLM alone sustain (R_vllm)? | k6 → vLLM |
| T1 | How much TTFB and duration does the gateway add at low load? | k6 → gateway → vLLM, paired with k6 → vLLM |
| T2 | What does the gateway do when the model is the bottleneck? | k6 → gateway (1 replica) → vLLM, 0.5–1.5 × R_vllm |
| T3 | What do real TTFB, token gaps and lengths look like? | samples from T0 and T2, plus a direct capture |
| T4 (optional) | Do 3 gateway replicas change anything? | as T2 with 3 replicas |
| T5 | Does the gateway change any answer? | `realmodel_eval.py`, direct and through the gateway |

Every k6 run sets `CONNECTION_REUSE=0` (#143).

## Time and cost

Prices from the AWS Price List on 2026-10-19, Singapore, On-Demand:

| Item | USD per hour |
|---|---|
| g4dn.xlarge model host | 0.736 (g4dn.2xlarge 1.052) |
| 2 × m6i.xlarge services nodes | 0.480 |
| c6i.xlarge load-generator node | 0.196 |
| EKS control plane | 0.100 |
| RDS db.t4g.small Single-AZ | 0.051 |
| NAT gateway (hours) | 0.059 |
| ALB with about one LCU | 0.033 |
| gp3 volumes (about 180 GB), public IPv4 | about 0.03 |
| **Total** | **about 1.69** |

Add NAT data at USD 0.059 per GB. The vLLM image (about 9 GB), the model (about
1.5 GB) and the service images come to about USD 0.65 once.

| Step | Time |
|---|---|
| Apply, model host ready, channel switched | 0.5–0.75 h |
| Compatibility gate | 0.25–0.5 h |
| T0 | 0.4 h |
| T1 (3 repeats) | 0.4 h |
| T2 (3 repeats of 5 levels, resets between) | 1 h |
| T3 direct capture | 0.25 h |
| T5 (strict, concurrency 32, under load) | 0.75 h |
| Evidence, teardown, leftover checks | 0.5–0.75 h |
| **Without T4** | **about 4.5 h, about USD 8.3** |
| T4 (optional) | +1 h, about +USD 1.7 |

If time runs short, do the gate, T1 and the strict T5 first: about 2.5 h, about
USD 4.8. Billing appears in Cost Explorer a day or two later; reconcile it then.

## 0. Preconditions

- The On-Demand G and VT vCPU quota in ap-southeast-1 is at least 4 for g4dn.xlarge,
  or 8 for g4dn.2xlarge. Check it with
  `aws service-quotas get-service-quota --service-code ec2 --quota-code L-DB2E81BA --region ap-southeast-1`.
- The AZ of private subnet 0 offers the type. If not, set `gpu_model_subnet_index = 1`:
  `aws ec2 describe-instance-type-offerings --region ap-southeast-1 --location-type availability-zone --filters Name=instance-type,Values=g4dn.xlarge`
- #144 and #145 are merged, and the session runs at a main commit that has both.
- The owner approved this paid session.

The shell variables of the [Flow example](README.md#flow-example) apply (`CD`, `K`,
`VALUES`, `EXPECT`). Add the model host's address once it exists (step 1):

```bash
KC=(--kubeconfig "$CD/eks.kubeconfig" --context <EKS context>)  # kubectl names it --context
mkdir -p "$CD/rm"
VLLM=$(terraform -chdir=deploy/terraform output -raw model_host_base_url)   # http://10.40.x.y:8000
MODEL=$(terraform -chdir=deploy/terraform output -raw model_host_served_model)  # qwen3-0.6b
PROMPTS=loadtest/prompts/realmodel.jsonl
RM=(--prompts "$PROMPTS" --env MODEL="$MODEL" --env CONNECTION_REUSE=0 --env TEMPERATURE=0.7 \
    --env TOTAL_DEADLINE_S=120 --env STREAM_S=30)
```

`TEMPERATURE=0.7` lets the answer lengths vary as in real traffic; T5 uses 0.
`TOTAL_DEADLINE_S=120` leaves queued calls room to finish once the model saturates.
The TTFB deadline stays at its 10 s default.

## 1. Apply with the model host

Add `gpu_model_enabled = true` to the ignored `experiment.tfvars`. Then plan and
apply as in the Terraform README, reviewing the plan first:

```bash
terraform -chdir=deploy/terraform plan -var-file=experiment.tfvars -out=experiment.tfplan
terraform -chdir=deploy/terraform apply experiment.tfplan
terraform -chdir=deploy/terraform output -json > "$CD/tf-output.json"
```

The reviewed plan must add, besides the usual environment, exactly these:

- `module.model_host[0]`: instance, security group, two ingress rules, one egress
  rule, role, policy attachment and instance profile;
- the AMI lookup.

The plan must show no CIDR ingress and `associate_public_ip_address = false`. Then
install or upgrade the chart as on a normal day.

## 2. vLLM health

The host needs about 10–15 minutes after the apply: boot, a 9 GB image pull through
the NAT, model download and CUDA graph capture. Open a shell with Session Manager,
which needs the Session Manager plugin for the aws CLI:

```bash
aws ssm start-session --region ap-southeast-1 --target "$(terraform -chdir=deploy/terraform output -raw model_host_instance_id)"
```

Inside the session:

```text
sudo tail -n 30 /var/log/gateflow-vllm.log
nvidia-smi
sudo docker ps
sudo docker logs --tail 50 vllm
curl -s localhost:8000/health -o /dev/null -w '%{http_code}\n'
curl -s localhost:8000/v1/models
curl -s localhost:8000/version
```

**Pass:**

- `nvidia-smi` shows a Tesla T4 and a driver of R580 or newer.
- The container is up, and its log reaches "Application startup complete" with no
  error.
- `/health` returns 200.
- `/v1/models` lists `qwen3-0.6b`.
- `/version` is 0.31.0.

Save the output of `nvidia-smi` and `docker logs` as evidence.

| Symptom | Fix (each one replaces the instance: plan and apply again) |
|---|---|
| Driver older than R580, or a CUDA error at start | `gpu_model_vllm_image = "vllm/vllm-openai:v0.31.0-cu129@sha256:ff29e51a9457f191deb332b96fa6440584fa941aa2a548449b604e2b1e5e1eeb"` |
| A crash in CUDA graph capture or the attention backend | `gpu_model_extra_args = ["--enforce-eager"]`, then also `"--attention-backend=TRITON_ATTN"` |
| Out of memory at start | `gpu_model_max_num_seqs = 128`, or `gpu_model_gpu_memory_utilization = 0.9` |
| The image or model download stalls | Check the NAT route of the private subnet; Docker Hub and Hugging Face must be reachable |

## 3. Point the gateway at vLLM

The gateway's seeded `ollama` channels take their URL and model from Helm values; no
chart change is needed. `--reuse-values` keeps the change through every later
`conditions.py upgrade`:

```bash
helm "${K[@]}" upgrade gf deploy/helm/gateflow -n gateflow --reuse-values \
  --set seedChannels.ollamaBaseURL="$VLLM" --set seedChannels.ollamaModel="$MODEL" --wait --wait-for-jobs --timeout 15m
```

`K` holds `--kubeconfig` and `--kube-context`, which helm accepts too. The post-upgrade
access seed replaces only the two channels; tenants, keys and wallets stay.

## 4. Compatibility gate

This is the first hard check; do not continue past a failure. Start the eval Pod on
the load-generator node, copy the script and prompts in, and run each protocol on two
items per set, directly and through the gateway:

```bash
python loadtest/realmodel_eval.py pod --name rm --lifetime 28800 --out "$CD/pod-rm.json"
kubectl "${KC[@]}" -n gateflow create -f "$CD/pod-rm.json"
kubectl "${KC[@]}" -n gateflow wait --for=condition=Ready pod/gateflow-eval-rm --timeout=300s
kubectl "${KC[@]}" -n gateflow cp loadtest/realmodel_eval.py gateflow-eval-rm:/tmp/realmodel_eval.py
kubectl "${KC[@]}" -n gateflow cp "$PROMPTS" gateflow-eval-rm:/tmp/realmodel.jsonl
EVAL=(kubectl "${KC[@]}" -n gateflow exec gateflow-eval-rm -- python /tmp/realmodel_eval.py)
RUN=(run --prompts /tmp/realmodel.jsonl --direct-url "$VLLM" --gateway-url http://gf-gateway:8080 --model "$MODEL")
"${EVAL[@]}" "${RUN[@]}" --out-dir /tmp/gate-messages --limit 2 --concurrency 1
"${EVAL[@]}" "${RUN[@]}" --out-dir /tmp/gate-responses --limit 2 --concurrency 1 --protocol responses
kubectl "${KC[@]}" -n gateflow cp gateflow-eval-rm:/tmp/gate-messages "$CD/rm/gate-messages"
kubectl "${KC[@]}" -n gateflow cp gateflow-eval-rm:/tmp/gate-responses "$CD/rm/gate-responses"
```

Then reconcile the bill. `reset_db.py` exports the ledger and reservations before it
empties them, which also gives T0 a clean start:

```bash
python deploy/aws/reset_db.py "${K[@]}" --evidence-dir "$CD/rm/reset-gate" --yes
python loadtest/realmodel_eval.py reconcile --answers "$CD/rm/gate-messages/answers-gateway.jsonl" --answers "$CD/rm/gate-responses/answers-gateway.jsonl" --ledger "$CD/rm/reset-gate/ledger_entries.csv" --reservations "$CD/rm/reset-gate/reservations.csv"
```

| Check | Pass |
|---|---|
| Messages, streaming, direct and through the gateway | Both passes 6/6 completed, `missing_usage` 0, `with_thinking` 0, verdict PASS (identical) |
| Responses, streaming, direct and through the gateway | The same, in `gate-responses/summary.md` |
| Billing | `reconcile` prints PASS: one `completed` ledger entry per gateway call, with the client's token counts and the amount of the reservation's prices, and `reservations_reserved` 0 |
| Mock channel unaffected | A short mock run, as on a normal day: `python loadtest/kube_run.py "${K[@]}" --name rm-mock --env LEVELS=2 --env HOLD_S=30 --env CONNECTION_REUSE=0` completes every request |

**If a protocol fails**, in this order:

1. Test with the protocol that works only, and say so in the results. For example,
   if Responses fails, T1–T5 still run on Messages, which is what k6 sends.
2. If Messages fails but Responses works, the k6 scenario cannot run as it is, since
   `messages.js` sends Messages only. Run T5 with `--protocol responses`, and record
   that T0–T2 are blocked.
3. Put LiteLLM between the gateway and vLLM to translate the protocol. Record the
   extra hop in every result, because its latency is inside the measurement. This
   needs a new reviewed change and is not part of this runbook.

If billing fails, stop: the S3 billing claims do not hold for this channel. Record the
mismatches and open a bug issue.

## 5. T0: vLLM capacity, direct

k6 aims at vLLM itself (`--gateway-url`), with the same prompts. One run is enough:

```bash
python deploy/aws/conditions.py drain "${K[@]}"
python loadtest/kube_run.py "${K[@]}" --name rm-t0 --gateway-url "$VLLM" "${RM[@]}" \
  --env LEVELS=2,5,10,15,20,25,30,40 --env WARMUP_S=60 --env RAMP_S=30 --env HOLD_S=120 --prealloc-vus 2000
```

**R_vllm** is the highest hold level with completion at least 99.5 %, which means
completed × 1000 ≥ 995 × scheduled. If even 40 passes, rerun with higher levels, such
as `40,60,80`. While it runs, watch the GPU in the Session Manager shell with
`nvidia-smi dmon -s u -d 5`, and keep that output as the utilisation timeline.

## 6. T1: added latency, paired

k6 sends every request both through the gateway and straight to vLLM, at 5 requests
per second. Run it three times:

```bash
for r in 1 2 3; do
  python deploy/aws/conditions.py drain "${K[@]}"
  python loadtest/kube_run.py "${K[@]}" --name rm-t1-r$r "${RM[@]}" --env PAIRED=1 --env DIRECT_URL="$VLLM" \
    --env LEVELS=5 --env WARMUP_S=60 --env HOLD_S=300
done
```

The results table gives added TTFB per pair (p50/p95/p99, negative values included)
and the direct durations. Pairing doubles the load on vLLM, so the rate stays far
below R_vllm.

## 7. T2: upstream saturation, one gateway

This test uses a fixed single gateway with the S3 configuration (the gateway-250m
overlay of cfg-D), and levels at 0.5, 0.8, 1.0, 1.2 and 1.5 times R_vllm, rounded.
For example, R_vllm = 20 gives `10,16,20,24,30`. Before every repeat, reset the
database: the reset exports the previous repeat's ledger for the billing check.

```bash
LEVELS_T2=<0.5,0.8,1.0,1.2,1.5 x R_vllm>
python deploy/aws/conditions.py upgrade "${K[@]}" "${VALUES[@]}" --terraform-output "$CD/tf-output.json" --fixed 1
for r in 1 2 3; do
  python deploy/aws/reset_db.py "${K[@]}" --evidence-dir "$CD/rm/reset-before-t2-r$r" --yes
  python deploy/aws/conditions.py drain "${K[@]}" --targets 1
  python deploy/aws/conditions.py run "${K[@]}" --name rm-t2-r$r --mode fixed --levels "$LEVELS_T2" -- "${RM[@]}" --prealloc-vus 3000
done
python deploy/aws/reset_db.py "${K[@]}" --evidence-dir "$CD/rm/reset-after-t2" --yes
```

The billing check of each repeat reads the reset that follows it: r1 in
`reset-before-t2-r2`, r2 in `reset-before-t2-r3`, r3 in `reset-after-t2`:

```bash
python loadtest/realmodel_eval.py reconcile --ledger "$CD/rm/reset-before-t2-r2/ledger_entries.csv" --reservations "$CD/rm/reset-before-t2-r2/reservations.csv"
```

**Pass:** `reconcile` prints PASS, with every amount equal to price × tokens and
`reservations_reserved` 0.

- The `completed` count of `outcomes` should match the completed results of the k6
  run.
- Past saturation, expect `client_cancelled` entries where k6 gave up at its
  deadline.

Read the results tables together with the gateway CPU from `prom/`, the k6 TTFB, and
the GPU timeline. The question is which gives way first:

- **vLLM:** the TTFB grows from queueing while the gateway CPU stays below its limit.
- **The gateway:** its CPU sits at the limit while vLLM still has headroom.

## 8. T3: distributions

The k6 JSON of T0 and T2 already holds per-request TTFB and duration. For token
gaps and output lengths, capture the direct stream at three concurrencies, with the
arrival time of every delta:

```bash
for c in 8 32 64; do
  "${EVAL[@]}" "${RUN[@]}" --passes direct --record-deltas --concurrency $c --out-dir /tmp/t3-c$c
  kubectl "${KC[@]}" -n gateflow cp gateflow-eval-rm:/tmp/t3-c$c "$CD/rm/t3-c$c"
done
```

Each line of `answers-direct.jsonl` has `ttfb_ms`, `first_output_ms`, `delta_ms` (one
arrival time per output delta) and `output_tokens`. These are the inputs of plan C's
mock profile.

## 9. T4 (optional): three gateway replicas

Run as T2 with `--fixed 3` and `--targets 3`, and names `rm-t4-r$r`. If T2 showed
vLLM giving way first, more gateway replicas should not raise the completed rate.

## 10. T5: output correctness

All runs are at temperature 0 and save every answer. The strict run is the gate. The
other two show behaviour under concurrency and under load:

```bash
"${EVAL[@]}" "${RUN[@]}" --concurrency 1 --out-dir /tmp/t5-strict
"${EVAL[@]}" "${RUN[@]}" --concurrency 32 --control --out-dir /tmp/t5-c32
for d in t5-strict t5-c32; do kubectl "${KC[@]}" -n gateflow cp gateflow-eval-rm:/tmp/$d "$CD/rm/$d"; done
```

For the run under load, start k6 at about 0.8 × R_vllm in a second terminal, wait
for its hold, then run the eval while the hold lasts:

```bash
python loadtest/kube_run.py "${K[@]}" --name rm-t5-load "${RM[@]}" --env LEVELS=<0.8 x R_vllm> --env WARMUP_S=60 --env HOLD_S=1200
"${EVAL[@]}" "${RUN[@]}" --concurrency 32 --control --out-dir /tmp/t5-load
kubectl "${KC[@]}" -n gateflow cp gateflow-eval-rm:/tmp/t5-load "$CD/rm/t5-load"
```

| Run | Pass |
|---|---|
| `t5-strict` | Exit 0: every call completed on both sides, and the direct and gateway answers are 100 % identical; the accuracy of the two sides is equal |
| `t5-c32` | No call left incomplete, and no gateway answer cut short (`stop_reasons` as direct). Identical rate at or above the `direct_vs_direct_repeat` rate. A difference only counts against the gateway beyond that noise floor |
| `t5-load` | The same as `t5-c32`, and accuracy no lower than `t5-c32` beyond the same noise |

The strict run uses one call at a time, so vLLM cannot batch it differently. Any
difference there is the gateway's.

Then reconcile the bill of the strict run. The reset also empties the tables before
teardown:

```bash
python deploy/aws/reset_db.py "${K[@]}" --evidence-dir "$CD/rm/reset-after-t5" --yes
python loadtest/realmodel_eval.py reconcile --answers "$CD/rm/t5-strict/answers-gateway.jsonl" --ledger "$CD/rm/reset-after-t5/ledger_entries.csv" --reservations "$CD/rm/reset-after-t5/reservations.csv"
```

`--exec-humaneval` would also run the HumanEval tests. Use it only in this
disposable Pod, never on a workstation, because the code comes from the model.

## 11. Evidence

Keep in `$CD/rm/`, next to the day's normal evidence:

- the `summary.md`, `summary.json` and answers of every eval run;
- the k6 run folders of `.local/loadtest/rm-*` with their `prom/` exports;
- the reset exports;
- `nvidia-smi`, `docker logs vllm`, `/version`, and the GPU timeline;
- the Terraform outputs, including `model_host_ami_id` and `model_host_image`.

Never copy a gateway key, the seed secret or a Job log of the access seed into
evidence.

## 12. Teardown and leftover checks

Delete the eval Pod, then tear down as on every day ([Daily
teardown](../terraform/README.md#daily-teardown-bibi45c-only)). The model host goes
with the destroy plan, and the inventory records it from the Terraform outputs:

```bash
kubectl "${KC[@]}" -n gateflow delete pod gateflow-eval-rm --ignore-not-found
terraform -chdir=deploy/terraform output -json > "$CD/tf-output.json"
python deploy/aws/leftovers.py inventory --terraform-output "$CD/tf-output.json"
terraform -chdir=deploy/terraform plan -destroy -var-file=experiment.tfvars -out=destroy.tfplan
terraform -chdir=deploy/terraform apply destroy.tfplan
python deploy/aws/leftovers.py check --inventory "$CD/teardown-inventory.json" --state-bucket <state bucket> --wait 1800
python deploy/aws/sweep.py
```

**Pass:**

- The destroy plan removes the eight `module.model_host[0]` resources with the rest.
- `leftovers.py check` exits 0 with `no billable leftovers found`.
- `sweep.py` exits 0 in every region: no instance, volume, NAT, EIP, load balancer,
  RDS or VPC is left.

Until both checks pass, the teardown is not complete. Set `gpu_model_enabled` back
to false in `experiment.tfvars`, so the next ordinary day does not bring the host
back.
