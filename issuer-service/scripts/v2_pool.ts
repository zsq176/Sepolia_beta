import { ethers } from "hardhat";
import * as https from "https";
import { upsertReport } from "./report";

const V2_FACTORY = "0xF62c03E08ada871A0bEb309762E260a7a6a880E6";
const V2_ROUTER = "0xeE567Fe1712Faf6149d80dA1E6934E354124CfE3";
const DEAD_ADDRESS = "0x000000000000000000000000000000000000dEaD";

const V2_FACTORY_ABI = [
  "function createPair(address tokenA, address tokenB) external returns (address pair)",
  "function getPair(address tokenA, address tokenB) external view returns (address pair)",
];

const V2_PAIR_ABI = [
  "function transfer(address to, uint256 amount) external returns (bool)",
  "function balanceOf(address owner) external view returns (uint256)",
  "function getReserves() external view returns (uint112 reserve0, uint112 reserve1, uint32 blockTimestampLast)",
  "function token0() external view returns (address)",
  "function token1() external view returns (address)",
];

const V2_ROUTER_ABI = [
  "function addLiquidity(address tokenA,address tokenB,uint amountADesired,uint amountBDesired,uint amountAMin,uint amountBMin,address to,uint deadline) external returns (uint amountA, uint amountB, uint liquidity)",
];

const ERC20_ABI = [
  "function approve(address spender, uint256 amount) external returns (bool)",
  "function decimals() external view returns (uint8)",
];

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

  const btcAddr = process.env.BTC_BETA_ADDR!;
  const usdtAddr = process.env.USDT_BETA_ADDR!;

  console.log("BTC-Beta:", btcAddr);
  console.log("USDT-Beta:", usdtAddr);

  const btc = new ethers.Contract(btcAddr, ERC20_ABI, deployer);
  const usdt = new ethers.Contract(usdtAddr, ERC20_ABI, deployer);

  const factory = new ethers.Contract(V2_FACTORY, V2_FACTORY_ABI, deployer);
  const router = new ethers.Contract(V2_ROUTER, V2_ROUTER_ABI, deployer);

  let pairAddr = await factory.getPair(btcAddr, usdtAddr);
  let createTxHash = "";
  if (pairAddr === ethers.ZeroAddress) {
    console.log("Creating V2 pair...");
    const tx = await factory.createPair(btcAddr, usdtAddr);
    const receipt = await tx.wait();
    createTxHash = receipt.hash;
    pairAddr = await factory.getPair(btcAddr, usdtAddr);
    console.log("V2 Pair created:", pairAddr);
  } else {
    console.log("V2 Pair already exists:", pairAddr);
  }

  const pair = new ethers.Contract(pairAddr, V2_PAIR_ABI, deployer);
  const btcDec = await btc.decimals();
  const usdtDec = await usdt.decimals();

  const btcUsdt = await fetchBinancePrice("BTCUSDT");
  const targetUsdt = 10_000_000;
  const btcAmountFloat = targetUsdt / btcUsdt;
  const btcAmount = ethers.parseUnits(btcAmountFloat.toFixed(Number(btcDec)), btcDec);
  const usdtAmount = ethers.parseUnits("10000000", usdtDec);
  console.log(`Binance BTCUSDT: ${btcUsdt}`);
  console.log(`Providing liquidity: ${ethers.formatUnits(btcAmount, btcDec)} BTC-Beta + ${ethers.formatUnits(usdtAmount, usdtDec)} USDT-Beta`);

  const reservesBefore = await pair.getReserves();
  const token0 = await pair.token0();
  const reserve0IsBTC = token0.toLowerCase() === btcAddr.toLowerCase();
  const reserveUSDT = reserve0IsBTC ? reservesBefore[1] : reservesBefore[0];
  const forceAdd = process.env.V2_FORCE_ADD === "1";
  let addTxHash = "(skipped: already funded)";
  if (forceAdd || reserveUSDT < usdtAmount) {
    const txApprove1 = await btc.approve(V2_ROUTER, btcAmount);
    await txApprove1.wait();
    console.log("BTC-Beta approved");

    const txApprove2 = await usdt.approve(V2_ROUTER, usdtAmount);
    await txApprove2.wait();
    console.log("USDT-Beta approved");

    const deadline = Math.floor(Date.now() / 1000) + 1800;
    const txAdd = await router.addLiquidity(
      btcAddr,
      usdtAddr,
      btcAmount,
      usdtAmount,
      0,
      0,
      deployer.address,
      deadline
    );
    const receipt = await txAdd.wait();
    addTxHash = receipt.hash;
    console.log("Liquidity added via Router02, tx:", receipt.hash);
  } else {
    console.log("Skipping addLiquidity: pool already has >= target USDT reserve (set V2_FORCE_ADD=1 to force)");
  }

  const lpBalance = await pair.balanceOf(deployer.address);
  console.log("LP balance:", lpBalance.toString());

  let burnHash = "(skipped: no LP to burn)";
  if (lpBalance > 0n) {
    console.log("Burning LP tokens to dead address...");
    const txBurn = await pair.transfer(DEAD_ADDRESS, lpBalance);
    const burnReceipt = await txBurn.wait();
    burnHash = burnReceipt.hash;
    console.log("LP Burn TxID:", burnReceipt.hash);
  } else {
    console.log("No LP balance on deployer, burn skipped");
  }

  const reserves = await pair.getReserves();
  const token0After = await pair.token0();
  const reserve0Label = token0After.toLowerCase() === btcAddr.toLowerCase() ? "BTC-Beta" : "USDT-Beta";
  const reserve1Label = reserve0Label === "BTC-Beta" ? "USDT-Beta" : "BTC-Beta";
  const reserve0Dec = reserve0Label === "BTC-Beta" ? btcDec : usdtDec;
  const reserve1Dec = reserve1Label === "BTC-Beta" ? btcDec : usdtDec;
  console.log("\n=== V2 Pool Summary ===");
  console.log("Pair:", pairAddr);
  console.log("Reserve0:", ethers.formatUnits(reserves[0], reserve0Dec), reserve0Label);
  console.log("Reserve1:", ethers.formatUnits(reserves[1], reserve1Dec), reserve1Label);
  console.log("LP Burn TxID:", burnHash);

  const report = upsertReport("v2_btc_usdt", {
    pair: pairAddr,
    create_pair_tx: createTxHash || "(already exists)",
    add_liquidity_tx: addTxHash,
    burn_lp_tx: burnHash,
    reserve0: reserves[0].toString(),
    reserve1: reserves[1].toString(),
    updated_at: Math.floor(Date.now() / 1000),
  });
  console.log("Report updated:", report);
}

main().catch(console.error);
