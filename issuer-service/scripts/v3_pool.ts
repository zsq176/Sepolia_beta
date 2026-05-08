import { ethers } from "hardhat";
import * as https from "https";
import { upsertReport } from "./report";

const V3_FACTORY = "0x0227628f3F023bb0B980b67D528571c95c6DaC1c";
const V3_POSITION_MANAGER = "0x1238536071E1c677A632429e3655c799b22cDA52";
const DEAD_ADDRESS = "0x000000000000000000000000000000000000dEaD";

const V3_FACTORY_ABI = [
  "function getPool(address tokenA, address tokenB, uint24 fee) external view returns (address pool)",
  "function createPool(address tokenA, address tokenB, uint24 fee) external returns (address pool)",
];

const V3_PM_ABI = [
  "function mint(tuple(address token0, address token1, uint24 fee, int24 tickLower, int24 tickUpper, uint256 amount0Desired, uint256 amount1Desired, uint256 amount0Min, uint256 amount1Min, address recipient, uint256 deadline) params) external payable returns (uint256 tokenId, uint128 liquidity, uint256 amount0, uint256 amount1)",
  "function safeTransferFrom(address from, address to, uint256 tokenId) external",
];

const ERC20_ABI = [
  "function approve(address spender, uint256 amount) external returns (bool)",
  "function decimals() external view returns (uint8)",
];

const POOL_ABI = [
  "function initialize(uint160 sqrtPriceX96) external",
  "function slot0() external view returns (uint160 sqrtPriceX96, int24 tick, uint16 observationIndex, uint16 observationCardinality, uint16 observationCardinalityNext, uint32 feeProtocol, bool unlocked)",
  "function token0() external view returns (address)",
  "function token1() external view returns (address)",
  "function liquidity() external view returns (uint128)",
];

function bigIntSqrt(value: bigint): bigint {
  if (value < 2n) return value;
  let x = value;
  let y = (x + 1n) / 2n;
  while (y < x) { x = y; y = (x + value / x) / 2n; }
  return x;
}

function encodePriceSqrt(reserve1: bigint, reserve0: bigint): bigint {
  return bigIntSqrt((reserve1 << 192n) / reserve0);
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

async function getBumpedFees(multiplierBps: bigint): Promise<{ maxFeePerGas: bigint; maxPriorityFeePerGas: bigint }> {
  const feeData = await ethers.provider.getFeeData();
  const fallbackPriority = ethers.parseUnits("2", "gwei");
  const basePriority = feeData.maxPriorityFeePerGas ?? fallbackPriority;
  const baseMaxFee = feeData.maxFeePerGas ?? (basePriority * 2n);
  return {
    maxPriorityFeePerGas: (basePriority * multiplierBps) / 10000n,
    maxFeePerGas: (baseMaxFee * multiplierBps) / 10000n,
  };
}

async function main() {
  const [deployer] = await ethers.getSigners();
  const btcAddr = process.env.BTC_BETA_ADDR!;
  const usdtAddr = process.env.USDT_BETA_ADDR!;

  console.log("BTC-Beta:", btcAddr);
  console.log("USDT-Beta:", usdtAddr);

  const factory = new ethers.Contract(V3_FACTORY, V3_FACTORY_ABI, deployer);
  const pm = new ethers.Contract(V3_POSITION_MANAGER, V3_PM_ABI, deployer);

  const btcDec = 8;
  const usdtDec = 6;
  const fee = 3000;
  const btcPrice = await fetchBinancePrice("BTCUSDT");
  console.log(`Binance BTCUSDT: ${btcPrice}`);

  let poolAddr = await factory.getPool(btcAddr, usdtAddr, fee);
  let createPoolTx = "";
  if (poolAddr === ethers.ZeroAddress) {
    console.log("Creating V3 pool (0.3% fee)...");
    const tx = await factory.createPool(btcAddr, usdtAddr, fee);
    const receipt = await tx.wait();
    createPoolTx = receipt.hash;
    poolAddr = await factory.getPool(btcAddr, usdtAddr, fee);
    console.log("V3 Pool created:", poolAddr);
  } else {
    console.log("V3 Pool exists:", poolAddr);
  }

  const pool = new ethers.Contract(poolAddr, POOL_ABI, deployer);
  const token0 = await pool.token0();
  const token1 = await pool.token1();
  const isBtcToken0 = token0.toLowerCase() === btcAddr.toLowerCase();
  console.log("Token0:", token0, isBtcToken0 ? "(BTC-Beta)" : "(USDT-Beta)");

  const slot = await pool.slot0();
  const tickSpacing = 60;

  if (slot.sqrtPriceX96 === 0n) {
    // Compute sqrtPriceX96 based on actual token0
    let sqrtPriceX96: bigint;
    if (isBtcToken0) {
      // price = USDT/BTC, so reserve1=USDT, reserve0=BTC
      sqrtPriceX96 = encodePriceSqrt(
        ethers.parseUnits(btcPrice.toFixed(Number(usdtDec)), usdtDec),
        ethers.parseUnits("1", btcDec)
      );
    } else {
      // price = BTC/USDT, so reserve1=BTC, reserve0=USDT
      sqrtPriceX96 = encodePriceSqrt(
        ethers.parseUnits("1", btcDec),
        ethers.parseUnits(btcPrice.toFixed(Number(usdtDec)), usdtDec)
      );
    }
    console.log("Initializing pool...");
    const txInit = await pool.initialize(sqrtPriceX96, { gasLimit: 200000 });
    const initReceipt = await txInit.wait();
    console.log("Pool initialized, tx:", txInit.hash);
    createPoolTx = createPoolTx || initReceipt.hash;
  } else {
    console.log("Pool already initialized, tick:", slot.tick);
  }

  // Read actual slot0 after init
  const slotAfter = await pool.slot0();
  const currentTick = Number(slotAfter.tick);
  const rangePercent = 0.5; // ±50% from current price

  // Compute tick range: ticks = log(1+range%) / log(1.0001)
  const tickRange = Math.floor(Math.log(1 + rangePercent) / Math.log(1.0001));
  const tickLower = Math.floor((currentTick - tickRange) / tickSpacing) * tickSpacing;
  const tickUpper = Math.floor((currentTick + tickRange) / tickSpacing) * tickSpacing;
  console.log("Current tick:", currentTick, "Range:", tickLower, "to", tickUpper);

  // Use same amounts as V2: 100 BTC + 10M USDT
  const targetUsdt = 10_000_000;
  const btcAmount = ethers.parseUnits((targetUsdt / btcPrice).toFixed(Number(btcDec)), btcDec);
  const usdtAmount = ethers.parseUnits("10000000", usdtDec);

  const forceAdd = process.env.V3_FORCE_ADD === "1";
  let mintTxHash = "(skipped)";
  let burnTxHash = "(skipped)";
  let tokenId: bigint = 0n;
  const currentLiquidity: bigint = await pool.liquidity();
  if (!forceAdd && currentLiquidity > 0n) {
    console.log("Skipping mint: pool already has liquidity (set V3_FORCE_ADD=1 to force)");
  } else {
    // Approve tokens
    const btc = new ethers.Contract(btcAddr, ERC20_ABI, deployer);
    const usdt = new ethers.Contract(usdtAddr, ERC20_ABI, deployer);
    await btc.approve(V3_POSITION_MANAGER, btcAmount).then((t: any) => t.wait());
    await usdt.approve(V3_POSITION_MANAGER, usdtAmount).then((t: any) => t.wait());
    console.log("Tokens approved");

    const deadline = Math.floor(Date.now() / 1000) + 1800;
    const params: any = {
      token0: token0,
      token1: token1,
      fee: fee,
      tickLower: tickLower,
      tickUpper: tickUpper,
      amount0Desired: isBtcToken0 ? btcAmount : usdtAmount,
      amount1Desired: isBtcToken0 ? usdtAmount : btcAmount,
      amount0Min: 0,
      amount1Min: 0,
      recipient: deployer.address,
      deadline: deadline,
    };

    console.log("Minting V3 position...");
    const txMint = await pm.mint(params, { gasLimit: 800000 });
    const receipt = await txMint.wait();
    mintTxHash = receipt.hash;

    // Extract tokenId from Transfer event
    const transferTopic = ethers.id("Transfer(address,address,uint256)");
    for (const log of receipt.logs) {
      if (
        log.address.toLowerCase() === V3_POSITION_MANAGER.toLowerCase() &&
        log.topics[0] === transferTopic &&
        log.topics[1] === ethers.zeroPadValue(ethers.ZeroAddress, 32)
      ) {
        tokenId = ethers.toBigInt(log.topics[3]);
        console.log("TokenId:", tokenId.toString());
        break;
      }
    }

    if (tokenId > 0n) {
      console.log("Burning V3 NFT to dead address...");
      let burnDone = false;
      let multiplier = 12000n; // 1.2x network suggested fee
      const maxAttempts = 4;
      for (let i = 0; i < maxAttempts; i++) {
        try {
          const fees = await getBumpedFees(multiplier);
          const txBurn = await pm.safeTransferFrom(deployer.address, DEAD_ADDRESS, tokenId, {
            nonce: await ethers.provider.getTransactionCount(deployer.address, "pending"),
            maxFeePerGas: fees.maxFeePerGas,
            maxPriorityFeePerGas: fees.maxPriorityFeePerGas,
          });
          const burnReceipt = await txBurn.wait();
          burnTxHash = burnReceipt.hash;
          console.log("V3 LP Burn TxID:", burnReceipt.hash);
          burnDone = true;
          break;
        } catch (e: any) {
          const msg = String(e?.message ?? e).toLowerCase();
          const retriable = msg.includes("replacement transaction underpriced") || msg.includes("nonce too low");
          if (!retriable || i === maxAttempts - 1) throw e;
          multiplier += 1500n; // +15% each retry
          console.log(`Burn tx gas too low, retrying with higher fees (attempt ${i + 2}/${maxAttempts})...`);
        }
      }
      if (!burnDone) {
        throw new Error("Failed to burn V3 LP NFT after fee bump retries");
      }
    }
  }

  console.log("\n=== V3 Pool Summary ===");
  console.log("Pool:", poolAddr);
  console.log("TokenId:", tokenId.toString());
  console.log("Fee: 0.3%");
  console.log("Tick range:", tickLower, "-", tickUpper);
  const report = upsertReport("v3_btc_usdt", {
    pool: poolAddr,
    create_pool_or_init_tx: createPoolTx || "(already exists)",
    mint_tx: mintTxHash,
    token_id: tokenId.toString(),
    burn_lp_tx: burnTxHash,
    updated_at: Math.floor(Date.now() / 1000),
  });
  console.log("Report updated:", report);
}

main().catch(console.error);
