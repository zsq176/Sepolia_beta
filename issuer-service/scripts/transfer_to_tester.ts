import { ethers } from "hardhat";

const ERC20_ABI = [
  "function transfer(address to, uint256 amount) external returns (bool)",
  "function decimals() external view returns (uint8)",
  "function balanceOf(address owner) external view returns (uint256)",
];

async function main() {
  const [deployer] = await ethers.getSigners();
  const testerAddr = process.env.TESTER_ADDRESS!;
  if (!testerAddr) {
    console.error("❌ Please set TESTER_ADDRESS env variable");
    process.exit(1);
  }

  const btcAddr = process.env.BTC_BETA_ADDR!;
  const usdtAddr = process.env.USDT_BETA_ADDR!;
  const ethAddr = process.env.ETH_BETA_ADDR!;

  const btc = new ethers.Contract(btcAddr, ERC20_ABI, deployer);
  const usdt = new ethers.Contract(usdtAddr, ERC20_ABI, deployer);
  const eth = new ethers.Contract(ethAddr, ERC20_ABI, deployer);

  const btcDec = Number(await btc.decimals());
  const usdtDec = Number(await usdt.decimals());
  const ethDec = Number(await eth.decimals());

  const btcAmount = ethers.parseUnits("10000000", btcDec); // 10M BTC-Beta
  const ethAmount = ethers.parseUnits("500000000", ethDec); // 500M ETH-Beta
  const usdtAmount = ethers.parseUnits("500000000", usdtDec); // 500M USDT-Beta

  console.log(`Transferring to: ${testerAddr}`);
  console.log(`BTC-Beta: ${ethers.formatUnits(btcAmount, btcDec)}`);
  console.log(`ETH-Beta: ${ethers.formatUnits(ethAmount, ethDec)}`);
  console.log(`USDT-Beta: ${ethers.formatUnits(usdtAmount, usdtDec)}`);

  const results: { token: string; txHash: string }[] = [];

  try {
    const tx1 = await btc.transfer(testerAddr, btcAmount);
    await tx1.wait();
    console.log("✅ BTC-Beta transfer:", tx1.hash);
    results.push({ token: "BTC-Beta", txHash: tx1.hash });
  } catch (e: any) {
    console.log("❌ BTC-Beta failed:", e.message?.slice(0, 100));
  }

  try {
    const tx2 = await eth.transfer(testerAddr, ethAmount);
    await tx2.wait();
    console.log("✅ ETH-Beta transfer:", tx2.hash);
    results.push({ token: "ETH-Beta", txHash: tx2.hash });
  } catch (e: any) {
    console.log("❌ ETH-Beta failed:", e.message?.slice(0, 100));
  }

  try {
    const tx3 = await usdt.transfer(testerAddr, usdtAmount);
    await tx3.wait();
    console.log("✅ USDT-Beta transfer:", tx3.hash);
    results.push({ token: "USDT-Beta", txHash: tx3.hash });
  } catch (e: any) {
    console.log("❌ USDT-Beta failed:", e.message?.slice(0, 100));
  }

  console.log("\n=== Transfer Summary ===");
  results.forEach(r => console.log(`  ${r.token}: ${r.txHash}`));
  console.log(`\nTotal transferred to ${testerAddr}`);
}

main().catch(console.error);
