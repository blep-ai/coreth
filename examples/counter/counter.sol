pragma solidity ^0.5.10;

contract Counter {
    uint256 x;

    constructor() public {
        x = 42;
    }

    function add(uint256 y) public returns (uint256) {
        x = x + y;
        return x;
    }
}
