import { PropsWithChildren } from 'react';

type props = PropsWithChildren<{}>;
export default function App({ children }: props) {
  return <div className=' w-full'>{children}</div>;
}
