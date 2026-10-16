#!/usr/bin/env python3
"""Actual signed gas top-up + token collection on a disposable local EVM only."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

import local_test as dev

ROOT = Path(__file__).resolve().parents[1]
dev.RPC = "http://127.0.0.1:18546"
dev.API = "http://127.0.0.1:18091"


def run():
    os.umask(0o077)
    project = "cryptopayments-sweep-test-" + secrets.token_hex(3)
    with tempfile.TemporaryDirectory(prefix="cryptopayments-sweep-") as directory:
        path = Path(directory)
        wallet = subprocess.run([str(ROOT / "bin/cryptopayments"), "wallet-init", str(path / "deposit.seed")], check=True, capture_output=True, text=True)
        gas = subprocess.run([str(ROOT / "bin/cryptopayments"), "gas-wallet-init", str(path / "gas.key")], check=True, capture_output=True, text=True)
        xpub = wallet.stdout.strip().split("=", 1)[1]
        gas_address = gas.stdout.strip().split("=", 1)[1]
        # Container UID can read the secrets; API does not mount these files.
        for name in ["deposit.seed", "gas.key"]:
            os.chown(path / name, 10001, 10001)
            (path / name).chmod(0o400)
        env = {**os.environ, "SWEEP_TEST_ENV_FILE": str(path / "test.env"), "SWEEP_TEST_SECRET_DIR": str(path)}
        compose = ["docker", "compose", "-p", project, "-f", str(ROOT / "compose.sweep-test.yaml")]
        values = {"CHAIN_ID": "97", "RPC_URL": "http://anvil:8545", "TOKEN_CONTRACT": "0x" + "1" * 40, "API_KEY": secrets.token_hex(32), "WALLET_XPUB": xpub, "CONFIRMATIONS": "3", "POLL_SECONDS": "1", "DATABASE_PATH": "/data/payments.sqlite", "AUTO_SWEEP_ENABLED": "true", "MAIN_WALLET_ADDRESS": "", "SWEEP_SEED_FILE": "/run/secrets/deposit.seed", "GAS_PRIVATE_KEY_FILE": "/run/secrets/gas.key", "SWEEP_POLL_SECONDS": "1"}

        def write_env():
            (path / "test.env").write_text("".join(f"{k}={v}\n" for k, v in values.items()))

        def start(*services):
            subprocess.run(compose + ["up", "-d"] + list(services), env=env, check=True, cwd=ROOT)

        write_env()
        try:
            start("anvil")
            dev.wait(lambda: dev.rpc("eth_chainId", []))
            dev.ensure_local()
            values["MAIN_WALLET_ADDRESS"] = dev.rpc("eth_accounts", [])[-1]
            values["TOKEN_CONTRACT"] = dev.deploy_token()
            write_env()
            start("api")
            dev.wait(lambda: dev.request(dev.API + "/readyz"))
            start("sweeper")
            auth = {"Authorization": "Bearer " + values["API_KEY"]}
            treasury = values["MAIN_WALLET_ADDRESS"]
            token = values["TOKEN_CONTRACT"]

            def api(url, body=None):
                return dev.request(dev.API + url, body, auth)

            def balance(address):
                data = "0x70a08231" + address[2:].rjust(64, "0")
                return int(dev.rpc("eth_call", [{"to": token, "data": data}, "latest"]), 16)

            def send(address, amount):
                data = "0xa9059cbb" + address[2:].rjust(64, "0") + f"{amount * 10**18:064x}"
                tx = dev.rpc("eth_sendTransaction", [{"from": dev.ACCOUNT, "to": token, "data": data, "gas": "0x20000"}])
                receipt = dev.wait(lambda: dev.rpc("eth_getTransactionReceipt", [tx]))
                assert receipt["status"] == "0x1"
                dev.rpc("anvil_mine", ["0x50"])

            def jobs(invoice):
                return api("/v1/invoices/" + invoice["id"] + "/sweeps")["sweeps"]

            def complete(invoice, count=1):
                def check():
                    dev.rpc("anvil_mine", ["0x50"])
                    all_jobs = jobs(invoice)
                    if len(all_jobs) == count and all(j["status"] == "completed" for j in all_jobs):
                        return all_jobs
                    return None
                return dev.wait(check, seconds=90)

            first = api("/v1/invoices", {"order_id": "auto-overpaid", "amount": "10"})
            assert int(dev.rpc("eth_getBalance", [first["address"], "latest"]), 16) == 0
            before = balance(treasury)
            send(first["address"], 12)
            dev.wait(lambda: "insufficient BNB" in api("/v1/status")["sweeper"].get("last_error", ""))
            assert jobs(first)[0]["transactions"] == [] and balance(treasury) == before
            print("PASS: unfunded gas wallet waits; no fake success or unsigned transfer")
            dev.rpc("eth_sendTransaction", [{"from": dev.ACCOUNT, "to": gas_address, "value": hex(10**18)}])

            def gas_prepared():
                all_jobs = jobs(first)
                if all_jobs and all_jobs[0]["transactions"]:
                    return all_jobs[0]["transactions"][0]["tx_hash"]
                return None
            gas_hash = dev.wait(gas_prepared)
            subprocess.run(compose + ["restart", "sweeper"], env=env, check=True, cwd=ROOT)
            finished = complete(first)
            assert finished[0]["transactions"][0]["tx_hash"] == gas_hash
            assert [t["kind"] for t in finished[0]["transactions"]] == ["gas", "token"]
            assert all(t["state"] == "confirmed" for t in finished[0]["transactions"])
            assert balance(treasury) - before == 12 * 10**18 and balance(first["address"]) == 0
            invoice = api("/v1/invoices/" + first["id"])
            assert invoice["received_amount"] == "12" and invoice["is_paid"] and invoice["status"] == "overpaid"
            print("PASS: zero-BNB child -> signed automatic BNB funding -> signed 12 USDT to main wallet")
            print("PASS: worker restart retains same gas transaction; invoice stays paid after collection")

            send(first["address"], 3)
            finished = complete(first, 2)
            assert sorted(j["amount"] for j in finished) == ["12", "3"]
            assert balance(treasury) - before == 15 * 10**18
            print("PASS: later deposit creates only a new 3-USDT sweep; no duplicate collection")

            second = api("/v1/invoices", {"order_id": "child-has-bnb", "amount": "10"})
            dev.rpc("eth_sendTransaction", [{"from": dev.ACCOUNT, "to": second["address"], "value": hex(10**16)}])
            send(second["address"], 10)
            finished = complete(second)
            assert [t["kind"] for t in finished[0]["transactions"]] == ["token"]
            assert balance(treasury) - before == 25 * 10**18
            print("PASS: child already has BNB -> token-only transfer; no gas top-up")

            partial = api("/v1/invoices", {"order_id": "partial-auto", "amount": "10"})
            send(partial["address"], 4)
            complete(partial)
            assert not api("/v1/invoices/" + partial["id"])["is_paid"]
            send(partial["address"], 6)
            complete(partial, 2)
            invoice = api("/v1/invoices/" + partial["id"])
            assert invoice["received_amount"] == "10" and invoice["is_paid"]
            assert balance(treasury) - before == 35 * 10**18
            print("PASS: partial 4 then 6 each collected once; payment total remains 10")

            subprocess.run(compose + ["restart", "sweeper", "api"], env=env, check=True, cwd=ROOT)
            dev.wait(lambda: dev.request(dev.API + "/readyz"))
            dev.rpc("anvil_mine", ["0x50"])
            time.sleep(3)
            assert balance(treasury) - before == 35 * 10**18
            assert len(jobs(first)) == 2 and len(jobs(second)) == 1 and len(jobs(partial)) == 2
            print("PASS: restart does not resend completed sweeps")
            print("All automatic-sweep EVM tests passed; no public-chain transactions or real funds.")
        finally:
            subprocess.run(compose + ["down", "--volumes", "--remove-orphans"], env=env, cwd=ROOT, check=False)


if __name__ == "__main__":
    run()
