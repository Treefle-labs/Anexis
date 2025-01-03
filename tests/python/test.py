#!/usr/bin/env python3
import sys
from cryptography.fernet import Fernet

def encrypt(input_file, output_file):
    with open(input_file, 'rb') as f:
        data = f.read()

    key = Fernet.generate_key()
    cipher = Fernet(key)
    encrypted_data = cipher.encrypt(data)

    # En production, il faudrait gérer la clé de manière sécurisée
    with open(f'{output_file}.key', 'wb') as f:
        f.write(key)

    with open(output_file, 'wb') as f:
        f.write(encrypted_data)

def decrypt(input_file, output_file):
    with open(input_file, 'rb') as f:
        encrypted_data = f.read()

    with open(f'{input_file}.key', 'rb') as f:
        key = f.read()

    cipher = Fernet(key)
    decrypted_data = cipher.decrypt(encrypted_data)

    with open(output_file, 'wb') as f:
        f.write(decrypted_data)

if __name__ == '__main__':
    operation = sys.argv[1]
    input_file = sys.argv[2]
    output_file = sys.argv[3]
    
    if operation == 'encrypt':
        encrypt(input_file, output_file)
    elif operation == 'decrypt':
        decrypt(input_file, output_file)