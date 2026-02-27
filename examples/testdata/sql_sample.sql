CREATE TABLE users (
    id INT PRIMARY KEY,
    name VARCHAR(255)
);

CREATE VIEW user_names AS SELECT name FROM users;
