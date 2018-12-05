// @flow
import * as React from 'react'
import * as Sb from '../../stories/storybook'
import {stringToAccountID} from '../../constants/types/wallets'
import Wallet from '.'
import header, {Container} from './header/index.stories'
import settings from './settings/index.stories'

const provider = Sb.createPropProviderWithCommon({
  // TODO mock out meaningful values once type `OwnProps` is defined
  Header: props => ({
    accountID: stringToAccountID('fakeAccountID'),
    isDefaultWallet: true,
    keybaseUser: 'cecileb',
    onReceive: Sb.action('onReceive'),
    onSendToAnotherAccount: Sb.action('onSendToAnotherAccount'),
    onSendToKeybaseUser: Sb.action('onSendToKeybaseUser'),
    onSendToStellarAddress: Sb.action('onSendToStellarAddress'),
    onSettings: Sb.action('onSettings'),
    onShowSecretKey: Sb.action('onShowSecretKey'),
    walletName: "cecileb's account",
  }),
})

const props = {
  accountID: stringToAccountID('fakeAccountID'),
  loadingMore: false,
  navigateAppend: Sb.action('navigateAppend'),
  navigateUp: Sb.action('navigateUp'),
  onLoadMore: Sb.action('onLoadMore'),
  onMarkAsRead: Sb.action('onMarkRead'),
  sections: [{data: [], title: 'Your assets'}, {data: ['noPayments'], title: 'History'}],
}

const load = () => {
  header()
  settings()
  Sb.storiesOf('Wallets/Wallet', module)
    .addDecorator(provider)
    .addDecorator(Container)
    .add('Default', () => <Wallet {...props} />)
}

export default load
