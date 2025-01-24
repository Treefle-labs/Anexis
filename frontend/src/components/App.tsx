'use server'

import { PropsWithChildren } from 'react';

type props = PropsWithChildren<{}>;
export default async function App({ children }: props) {
  return <div className=' w-full'>{children}</div>;
}
