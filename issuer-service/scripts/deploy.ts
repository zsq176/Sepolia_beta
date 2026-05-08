import { ethers } from "hardhat";
import { reportPath, upsertReport } from "./report";

async function main() {
  const [deployer] = await ethers.getSigners();
  console.log("Deployer:", deployer.address);
  console.log("Balance:", ethers.formatEther(await ethers.provider.getBalance(deployer.address)), "ETH\n");

  const BTCBeta = await ethers.getContractFactory("BTCBeta");
  const USDTBeta = await ethers.getContractFactory("USDTBeta");
  const ETHBeta = await ethers.getContractFactory("ETHBeta");

  let btcAddr = process.env.BTC_BETA_ADDR;
  let usdtAddr = process.env.USDT_BETA_ADDR;
  let ethAddr = process.env.ETH_BETA_ADDR;
  let btc: any;
  let usdt: any;
  let eth: any;

  if (btcAddr) {
    btc = BTCBeta.attach(btcAddr);
    console.log("BTC-Beta (reuse):", btcAddr);
  } else {
    btc = await BTCBeta.deploy();
    await btc.waitForDeployment();
    btcAddr = await btc.getAddress();
    console.log("BTC-Beta (new):", btcAddr);
  }

  if (usdtAddr) {
    usdt = USDTBeta.attach(usdtAddr);
    console.log("USDT-Beta (reuse):", usdtAddr);
  } else {
    usdt = await USDTBeta.deploy();
    await usdt.waitForDeployment();
    usdtAddr = await usdt.getAddress();
    console.log("USDT-Beta (new):", usdtAddr);
  }

  if (ethAddr) {
    eth = ETHBeta.attach(ethAddr);
    console.log("ETH-Beta (reuse):", ethAddr);
  } else {
    eth = await ETHBeta.deploy();
    await eth.waitForDeployment();
    ethAddr = await eth.getAddress();
    console.log("ETH-Beta (new):", ethAddr);
  }

  console.log("\n=== Token Supply ===");
  console.log("BTC-Beta:", ethers.formatUnits(await btc.totalSupply(), 8));
  console.log("USDT-Beta:", ethers.formatUnits(await usdt.totalSupply(), 6));
  console.log("ETH-Beta:", ethers.formatUnits(await eth.totalSupply(), 18));

  const output = upsertReport("tokens", {
    deployer: deployer.address,
    btc_beta: btcAddr!,
    usdt_beta: usdtAddr!,
    eth_beta: ethAddr!,
    chain_id: Number((await ethers.provider.getNetwork()).chainId),
    generated_at: Math.floor(Date.now() / 1000),
  });
  console.log("\nReport updated:", output);
  console.log("Report path:", reportPath());
}

main().catch(console.error);
