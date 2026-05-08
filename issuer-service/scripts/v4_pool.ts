import { ethers } from "hardhat";
import { Percent, Token } from "@uniswap/sdk-core";
import { Pool, Position, V4PositionManager } from "@uniswap/v4-sdk";
import * as https from "https";
import { upsertReport } from "./report";

const CHAIN_ID = 11155111;
const V4_POOL_MANAGER = "0xE03A1074c86CFeDd5C142C4F04F1a1536e203543";
const V4_POSITION_MANAGER = "0x429ba70129df741b2ca2a85bc3a2a3328e5c09b4";
const PERMIT2 = "0x000000000022D473030F116dDEE9F6B43aC78BA3";
const DEAD_ADDRESS = "0x000000000000000000000000000000000000dEaD";
const HOOKS = "0x0000000000000000000000000000000000000000";
const Q96 = 2n ** 96n;

const ERC20_ABI = [
  "function approve(address spender, uint256 amount) external returns (bool)",
];

const PERMIT2_ABI = [
  "function approve(address token, address spender, uint160 amount, uint48 expiration) external",
];

const POSITION_MANAGER_ABI = [
  "function modifyLiquidities(bytes unlockData, uint256 deadline) external payable",
  "function transferFrom(address from, address to, uint256 tokenId) external",
  "event Transfer(address indexed from, address indexed to, uint256 indexed tokenId)",
];

function sortTokens(a: string, b: string): [string, string] {
  return a.toLowerCase() < b.toLowerCase() ? [a, b] : [b, a];
}

function sqrt(value: bigint): bigint {
  if (value < 2n) return value;
  let x = value;
  let y = (x + 1n) / 2n;
  while (y < x) {
    x = y;
    y = (x + value / x) / 2n;
  }
  return x;
}

function encodePriceSqrt(reserve1: bigint, reserve0: bigint): bigint {
  const ratioX192 = (reserve1 << 192n) / reserve0;
  return sqrt(ratioX192);
}

function getTickAtSqrtPrice(sqrtPriceX96: bigint): number {
  const price = Number(sqrtPriceX96) / Number(Q96);
  const realPrice = price * price;
  return Math.floor(Math.log(realPrice) / Math.log(1.0001));
}

async function fetchBinancePrice(symbol: string): Promise<number> {
  const url = `https://api.binance.com/api/v3/ticker/price?symbol=${symbol}`;
  return new Promise((resolve, reject) => {
    https
      .get(url, (res) => {
        let data = "";
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => {
          try {
            const parsed = JSON.parse(data);
            const price = Number(parsed?.price);
            if (!Number.isFinite(price) || price <= 0) {
              reject(new Error(`Invalid Binance price for ${symbol}: ${data}`));
              return;
            }
            resolve(price);
          } catch (e) {
            reject(e);
          }
        });
      })
      .on("error", reject);
  });
}

async function main() {
  const [deployer] = await ethers.getSigners();
  console.log("Deployer:", deployer.address);
  console.log("V4 PositionManager:", V4_POSITION_MANAGER);

  const btcAddr = process.env.BTC_BETA_ADDR!;
  const usdtAddr = process.env.USDT_BETA_ADDR!;
  const ethAddr = process.env.ETH_BETA_ADDR!;

  const btcDec = 8;
  const usdtDec = 6;
  const ethDec = 18;

  const tokenMap: Record<string, Token> = {
    [btcAddr.toLowerCase()]: new Token(CHAIN_ID, btcAddr, btcDec, "BTC-Beta", "BTC-Beta"),
    [usdtAddr.toLowerCase()]: new Token(CHAIN_ID, usdtAddr, usdtDec, "USDT-Beta", "USDT-Beta"),
    [ethAddr.toLowerCase()]: new Token(CHAIN_ID, ethAddr, ethDec, "ETH-Beta", "ETH-Beta"),
  };

  interface PoolSetup {
    label: string;
    tokenA: string;
    tokenB: string;
    amountA: bigint;
    amountB: bigint;
    reserve1: bigint;
    reserve0: bigint;
  }

  const btcUsdt = await fetchBinancePrice("BTCUSDT");
  const ethUsdt = await fetchBinancePrice("ETHUSDT");
  const ethBtc = ethUsdt / btcUsdt;
  console.log(`Binance BTCUSDT: ${btcUsdt}`);
  console.log(`Binance ETHUSDT: ${ethUsdt}`);
  console.log(`Derived ETHBTC: ${ethBtc}`);

  const setups: PoolSetup[] = [
    {
      label: "BTC-Beta/USDT-Beta",
      tokenA: btcAddr,
      tokenB: usdtAddr,
      amountA: ethers.parseUnits((10_000_000 / btcUsdt).toFixed(Number(btcDec)), btcDec),
      amountB: ethers.parseUnits("10000000", usdtDec),
      reserve1: ethers.parseUnits(btcUsdt.toFixed(Number(usdtDec)), usdtDec),
      reserve0: ethers.parseUnits("1", btcDec),
    },
    {
      label: "ETH-Beta/USDT-Beta",
      tokenA: ethAddr,
      tokenB: usdtAddr,
      amountA: ethers.parseUnits((10_000_000 / ethUsdt).toFixed(6), ethDec),
      amountB: ethers.parseUnits("10000000", usdtDec),
      reserve1: ethers.parseUnits(ethUsdt.toFixed(Number(usdtDec)), usdtDec),
      reserve0: ethers.parseUnits("1", ethDec),
    },
    {
      label: "ETH-Beta/BTC-Beta",
      tokenA: ethAddr,
      tokenB: btcAddr,
      amountA: ethers.parseUnits((10_000_000 / ethUsdt).toFixed(6), ethDec),
      amountB: ethers.parseUnits((10_000_000 / btcUsdt).toFixed(Number(btcDec)), btcDec),
      reserve1: ethers.parseUnits(ethBtc.toFixed(Number(btcDec)), btcDec),
      reserve0: ethers.parseUnits("1", ethDec),
    },
  ];

  const permit2 = new ethers.Contract(PERMIT2, PERMIT2_ABI, deployer);
  const posm = new ethers.Contract(V4_POSITION_MANAGER, POSITION_MANAGER_ABI, deployer);

  const abiCoder = new ethers.AbiCoder();
  const initSel = ethers.id("initialize((address,address,uint24,int24,address),uint160)").slice(0, 10);
  const transferTopic = ethers.id("Transfer(address,address,uint256)");

  const permitAmount = (1n << 160n) - 1n;
  const permitExpiration = (1n << 48n) - 1n;
  const slippage = new Percent(50, 10_000); // 0.5%

  const results: { pool: string; initTx: string; liqTx: string; tokenId: string; burnTx: string }[] = [];
  const forceAdd = process.env.V4_FORCE_ADD === "1";

  for (const s of setups) {
    console.log(`\n=== ${s.label} ===`);
    const [c0, c1] = sortTokens(s.tokenA, s.tokenB);
    const isA0 = c0.toLowerCase() === s.tokenA.toLowerCase();
    const amount0Raw = isA0 ? s.amountA : s.amountB;
    const amount1Raw = isA0 ? s.amountB : s.amountA;
    const r1 = isA0 ? s.reserve1 : s.reserve0;
    const r0 = isA0 ? s.reserve0 : s.reserve1;

    const fee = 3000;
    const tickSpacing = 60;
    console.log(`  c0: ${c0.slice(0, 10)}...  c1: ${c1.slice(0, 10)}...`);

    const sqrtPX96 = encodePriceSqrt(r1, r0);
    const keyEncoded = abiCoder.encode(
      ["address", "address", "uint24", "int24", "address"],
      [c0, c1, fee, tickSpacing, HOOKS]
    );
    const initData = initSel + keyEncoded.slice(2) + abiCoder.encode(["uint160"], [sqrtPX96]).slice(2);

    let initTxHash = "(already initialized)";
    try {
      const tx = await deployer.sendTransaction({ to: V4_POOL_MANAGER, data: initData, gasLimit: 500000 });
      const receipt = await tx.wait();
      initTxHash = receipt?.hash ?? tx.hash;
      console.log("  ✅ Initialized:", initTxHash);
    } catch (e: any) {
      console.log("  Pool already initialized");
    }

    const erc0 = new ethers.Contract(c0, ERC20_ABI, deployer);
    const erc1 = new ethers.Contract(c1, ERC20_ABI, deployer);
    await erc0.approve(PERMIT2, amount0Raw).then((t: any) => t.wait());
    await erc1.approve(PERMIT2, amount1Raw).then((t: any) => t.wait());
    await permit2.approve(c0, V4_POSITION_MANAGER, permitAmount, permitExpiration).then((t: any) => t.wait());
    await permit2.approve(c1, V4_POSITION_MANAGER, permitAmount, permitExpiration).then((t: any) => t.wait());
    console.log("  Approved ERC20 + Permit2");

    const currentTick = getTickAtSqrtPrice(sqrtPX96);
    const tickRange = Math.floor(Math.log(1.5) / Math.log(1.0001));
    const tickLower = Math.floor((currentTick - tickRange) / tickSpacing) * tickSpacing;
    const tickUpper = Math.floor((currentTick + tickRange) / tickSpacing) * tickSpacing;
    console.log(`  Current tick: ${currentTick}, Range: [${tickLower}, ${tickUpper}]`);

    const pool = new Pool(
      tokenMap[c0.toLowerCase()],
      tokenMap[c1.toLowerCase()],
      fee,
      tickSpacing,
      HOOKS,
      sqrtPX96.toString(),
      "1",
      currentTick
    );

    const position = Position.fromAmounts({
      pool,
      tickLower,
      tickUpper,
      amount0: amount0Raw.toString(),
      amount1: amount1Raw.toString(),
      useFullPrecision: true,
    });

    const deadline = Math.floor(Date.now() / 1000) + 1800;
    const params = V4PositionManager.addCallParameters(position, {
      recipient: deployer.address,
      slippageTolerance: slippage,
      deadline,
      createPool: false,
      hookData: "0x",
    });

    let liqHash = "(skipped)";
    let burnHash = "(skipped)";
    let tokenId = 0n;
    if (!forceAdd && initTxHash === "(already initialized)") {
      console.log("  Skipping liquidity add by default for initialized pool (set V4_FORCE_ADD=1 to force)");
    } else {
      const txLiq = await deployer.sendTransaction({
        to: V4_POSITION_MANAGER,
        data: params.calldata,
        value: params.value,
        gasLimit: 5_000_000,
      });
      const liqReceipt = await txLiq.wait();
      if (liqReceipt?.status !== 1) {
        throw new Error(`Liquidity tx reverted: ${liqReceipt?.hash}`);
      }
      liqHash = liqReceipt.hash;
      console.log("  ✅ Liquidity added:", liqReceipt.hash);

      for (const log of liqReceipt.logs) {
        if (
          log.address.toLowerCase() === V4_POSITION_MANAGER.toLowerCase() &&
          log.topics[0] === transferTopic &&
          log.topics[1] === ethers.zeroPadValue(ethers.ZeroAddress, 32)
        ) {
          tokenId = ethers.toBigInt(log.topics[3]);
          break;
        }
      }
      if (tokenId === 0n) {
        throw new Error("Minted V4 tokenId not found in receipt");
      }
      console.log("  TokenId:", tokenId.toString());

      const txBurn = await posm.transferFrom(deployer.address, DEAD_ADDRESS, tokenId, { gasLimit: 500000 });
      const burnReceipt = await txBurn.wait();
      if (burnReceipt?.status !== 1) {
        throw new Error(`V4 burn tx reverted: ${burnReceipt?.hash}`);
      }
      burnHash = burnReceipt.hash;
      console.log("  ✅ V4 LP Burn TxID:", burnReceipt.hash);
    }

    results.push({
      pool: s.label,
      initTx: initTxHash,
      liqTx: liqHash,
      tokenId: tokenId.toString(),
      burnTx: burnHash,
    });
  }

  console.log("\n========================");
  console.log("V4 NFT LP Burn Records:");
  console.log(`  PositionManager: ${V4_POSITION_MANAGER}`);
  results.forEach((r) => {
    console.log(`  ${r.pool}`);
    console.log(`    Init:   ${r.initTx}`);
    console.log(`    LiqTx:  ${r.liqTx}`);
    console.log(`    TokenId:${r.tokenId}`);
    console.log(`    BurnTx: ${r.burnTx}`);
  });
  console.log("========================\n");
  const report = upsertReport("v4_pools", {
    pool_manager: V4_POOL_MANAGER,
    position_manager: V4_POSITION_MANAGER,
    records: JSON.stringify(results),
    updated_at: Math.floor(Date.now() / 1000),
  });
  console.log("Report updated:", report);
}

main().catch(console.error);
