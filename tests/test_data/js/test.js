class User {
  constructor(name) {
    this.name = name;
  }
  sayHello() {
    console.log("Hello " + this.name);
  }
}

function processUser(u) {
  u.sayHello();
}
