#!/usr/bin/env python3
"""Setup and exercise the API against a real local EVM, with valueless tokens.

Requires Docker Compose, Python 3 and a built bin/cryptopayments binary.
Refuses to send transactions unless the node identifies itself as Anvil, chain 97.
"""
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose", "-p", "cryptopayments-go", "-f", str(ROOT / "compose.yaml"), "-f", str(ROOT / "compose.local.yaml")]
RPC = "http://127.0.0.1:18545"
API = "http://127.0.0.1:18090"
ACCOUNT = "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"  # Public Anvil development account.


def request(url, body=None, headers=None):
    payload = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json", **(headers or {})})
    with urllib.request.urlopen(req, timeout=25) as response:
        return json.load(response)


def rpc(method, params):
    response = request(RPC, {"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
    if "error" in response:
        raise RuntimeError(response["error"])
    return response["result"]


def wait(check, seconds=45):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (OSError, ValueError, RuntimeError):
            pass
        time.sleep(0.3)
    raise RuntimeError("Timed out waiting for local development service")


def ensure_local():
    assert int(rpc("eth_chainId", []), 16) == 97, "Not chain 97"
    assert "anvil" in rpc("web3_clientVersion", []).lower(), "Refusing transactions outside local Anvil"


def read_env():
    values = dict(line.split("=", 1) for line in (ROOT / ".env").read_text().splitlines() if line and not line.startswith("#"))
    assert values.get("LOCAL_DEVELOPMENT_ONLY") == "true", "Refusing to use a non-development .env"
    return values


def write_env(values):
    path = ROOT / ".env"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write("".join(f"{key}={value}\n" for key, value in values.items()))


def deploy_token():
    ensure_local()
    bytecode = (ROOT / "contracts/build/contracts_TestUSDT_sol_TestUSDT.bin").read_text().strip()
    tx = rpc("eth_sendTransaction", [{"from": ACCOUNT, "data": "0x" + bytecode, "gas": "0x300000"}])
    receipt = wait(lambda: rpc("eth_getTransactionReceipt", [tx]))
    assert receipt["status"] == "0x1", "Test token deployment failed"
    return receipt["contractAddress"]


def setup():
    path = ROOT / ".env"
    if path.exists():
        values = read_env()
    else:
        wallet_dir = Path.home() / ".local/share/cryptopayments/local"
        wallet_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        public_file = wallet_dir / "wallet.xpub"
        if not public_file.exists():
            result = subprocess.run([str(ROOT / "bin/cryptopayments"), "wallet-init", str(wallet_dir / "wallet.seed")], check=True, capture_output=True, text=True)
            public_file.write_text(result.stdout.strip().split("=", 1)[1])
            public_file.chmod(0o600)
        values = {"LOCAL_DEVELOPMENT_ONLY": "true", "CHAIN_ID": "97", "RPC_URL": "http://anvil:8545", "TOKEN_CONTRACT": "0x" + "1" * 40, "API_KEY": secrets.token_hex(32), "WALLET_XPUB": public_file.read_text().strip(), "CONFIRMATIONS": "3", "POLL_SECONDS": "1", "INVOICE_TTL_SECONDS": "3600", "DATABASE_PATH": "/data/payments.sqlite"}
        write_env(values)
    subprocess.run(COMPOSE + ["up", "-d", "anvil"], check=True, cwd=ROOT)
    wait(lambda: rpc("eth_chainId", []))
    ensure_local()
    if rpc("eth_getCode", [values["TOKEN_CONTRACT"], "latest"]) == "0x":
        values["TOKEN_CONTRACT"] = deploy_token()
        write_env(values)
    subprocess.run(COMPOSE + ["up", "-d", "--build", "api"], check=True, cwd=ROOT)
    wait(lambda: request(API + "/readyz"), seconds=60)
    print("Local API ready: " + API + "; authentication in .env; local EVM only, no real funds.")


def test():
    values = read_env()
    ensure_local()
    auth = {"Authorization": "Bearer " + values["API_KEY"]}
    run = secrets.token_hex(5)

    def invoice(name, amount):
        return request(API + "/v1/invoices", {"order_id": f"e2e-{run}-{name}", "amount": amount}, auth)

    def get(i):
        return request(API + "/v1/invoices/" + i["id"], headers=auth)

    def transfer(token, address, units):
        # transfer(address,uint256), 18 decimal units; standard ERC20/BEP20 ABI.
        data = "0xa9059cbb" + address[2:].rjust(64, "0") + f"{units:064x}"
        tx = rpc("eth_sendTransaction", [{"from": ACCOUNT, "to": token, "data": data, "gas": "0x20000"}])
        receipt = wait(lambda: rpc("eth_getTransactionReceipt", [tx]))
        assert receipt["status"] == "0x1", "Transfer reverted"

    def settle(i, status):
        # Anvil's finalized tag trails by 64 blocks; exercise actual finality,
        # not only the API's three-block confirmation depth.
        rpc("anvil_mine", ["0x50"])
        result = wait(lambda: (v if (v := get(i))["status"] == status else None))
        assert result["chain_id"] == 97 and result["asset"] == "TEST-USDT"
        assert result["is_paid"] == (status in {"paid", "overpaid", "paid_late", "overpaid_late"})
        return result

    a = invoice("exact", "10")
    b = invoice("split", "10")
    over = invoice("over", "10")
    wrong = invoice("wrong-token", "10")
    assert len({i["address"] for i in [a, b, over, wrong]}) == 4
    assert get(a)["status"] == "pending" and get(a)["received_amount"] == "0" and not get(a)["is_paid"]
    assert invoice("exact", "10")["id"] == a["id"]
    try:
        invoice("exact", "11")
        raise AssertionError("Different amount reused an order_id")
    except urllib.error.HTTPError as error:
        assert error.code == 409
    print("PASS: unique addresses, unpaid status, idempotent order, amount conflict")

    transfer(values["TOKEN_CONTRACT"], a["address"], 10 * 10**18)
    assert settle(a, "paid")["received_amount"] == "10"
    print("PASS: actual EVM token transfer -> confirmed paid")
    transfer(values["TOKEN_CONTRACT"], b["address"], 4 * 10**18)
    assert settle(b, "partially_paid")["received_amount"] == "4"
    transfer(values["TOKEN_CONTRACT"], b["address"], 6 * 10**18)
    assert settle(b, "paid")["received_amount"] == "10"
    print("PASS: split transfers 4 + 6 -> paid 10")
    transfer(values["TOKEN_CONTRACT"], over["address"], 12 * 10**18)
    assert settle(over, "overpaid")["received_amount"] == "12"
    print("PASS: overpayment 12 -> overpaid")
    other_token = deploy_token()
    transfer(other_token, wrong["address"], 10 * 10**18)
    rpc("anvil_mine", ["0x50"])
    time.sleep(2)
    assert get(wrong)["received_amount"] == "0" and get(wrong)["status"] == "pending"
    print("PASS: same-symbol token from a different contract ignored")
    subprocess.run(COMPOSE + ["restart", "api"], check=True, cwd=ROOT)
    wait(lambda: request(API + "/readyz"))
    assert get(a)["received_amount"] == "10" and len(get(a)["transfers"]) == 1
    assert invoice("after-restart", "1")["address"] not in {a["address"], b["address"], over["address"], wrong["address"]}
    print("PASS: restart preserves payment, transfer uniqueness and address allocation")
    print("All local EVM integration tests passed. Public BSC Testnet is a separate test.")


if __name__ == "__main__":
    os.umask(0o077)
    command = sys.argv[1] if len(sys.argv) > 1 else "test"
    if command == "setup":
        setup()
    elif command == "test":
        test()
    else:
        raise SystemExit("usage: python3 scripts/local_test.py [setup|test]")
