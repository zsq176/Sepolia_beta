// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import "@openzeppelin/contracts/access/Ownable.sol";

contract BTCBeta is ERC20, Ownable {
    constructor() ERC20("BTC-Beta", "BTC-Beta") Ownable(msg.sender) {
        _mint(msg.sender, 21_000_000 * 10 ** decimals());
    }

    function decimals() public pure override returns (uint8) {
        return 8;
    }

    function mint(address to, uint256 amount) external onlyOwner {
        _mint(to, amount);
    }
}
